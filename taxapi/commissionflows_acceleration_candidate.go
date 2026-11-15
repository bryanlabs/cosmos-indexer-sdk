package taxapi

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// RunCommissionCaptureCandidate is an outer-root scheduler for a fresh private
// version. RunRoot must run the unchanged pinned native collector with ctx.
// Existing V2 queues remain untouched and CAN later overlap this queue. Reuse
// is checked immediately before work, and every overlap stays separate evidence.
// A dedicated capped read-only pool is required. No migration is performed here.
func (s *Server) RunCommissionCaptureCandidate(parent context.Context, target CommissionFlowBuildOptions, sources, canonical, roots []string, chain string, from time.Time, output, samplePath string, runRoot func(context.Context, string) error) error {
	if err := target.validate(); err != nil {
		return err
	}
	if target.PrivateProfile == nil || target.SourceCorrections == nil || len(canonical) != 324 || len(roots) == 0 || runRoot == nil {
		return fmt.Errorf("candidate requires exact corrected profile and complete canonical membership")
	}
	allowed := map[string]bool{}
	for _, v := range canonical {
		if v == "" || allowed[v] {
			return fmt.Errorf("invalid canonical candidate membership")
		}
		allowed[v] = true
	}
	selected := map[string]bool{}
	for _, v := range roots {
		if !allowed[v] || selected[v] {
			return fmt.Errorf("invalid candidate queue")
		}
		selected[v] = true
	}
	if len(sources) == 0 || len(sources) > 8 {
		return fmt.Errorf("explicit bounded source versions required")
	}
	seen := map[string]bool{target.Version: true}
	for _, v := range sources {
		if v == "" || seen[v] {
			return fmt.Errorf("invalid candidate source allowlist")
		}
		seen[v] = true
	}
	pool, err := s.db.DB()
	if err != nil || pool.Stats().MaxOpenConnections < 1 || pool.Stats().MaxOpenConnections > 2 {
		return fmt.Errorf("candidate requires dedicated pool capped at one or two connections")
	}
	ctx, stop, err := startCommissionAccelerationGuard(parent, func() (CommissionAccelerationHeadroom, error) { return readAccelerationSample(samplePath) }, time.Second)
	if err != nil {
		return err
	}
	defer stop()
	reader := *s
	reader.db = s.db.WithContext(ctx)
	failed := 0
	for _, v := range roots {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("candidate yielded before next root")
		}
		exists, ownErr := reader.reuseCommissionFlowVersion(target, chain, v, from)
		if ownErr == nil && exists {
			continue
		}
		ref, err := reader.ResolveCommissionCapture(target, sources, chain, v, from)
		if err != nil {
			failed++
			continue
		}
		if ref != nil {
			if err := retainCommissionAdoption(output, *ref); err != nil {
				return err
			}
			continue
		}
		if ownErr != nil {
			failed++
			continue
		}
		if err := runRoot(ctx, v); err != nil {
			failed++
			continue
		}
	}
	if failed > 0 {
		return fmt.Errorf("%d candidate roots incomplete; all attempts retained", failed)
	}
	return nil
}

// RunPinnedCommissionRoot executes the original binary, not a rebuilt native
// computation. A new wrapper image may supply this scheduler via init copy while
// the collector container retains fc277. The executable digest and reviewed args
// belong in Main's exact Root. Each native attempt gets a fresh retained folder.
func RunPinnedCommissionRoot(ctx context.Context, binary, digest string, args []string, root, output string, cancelNative func(string) error) error {
	if !filepath.IsAbs(binary) || !correctionHash(digest) || root == "" {
		return fmt.Errorf("invalid pinned native executor")
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--commission-validators") || strings.HasPrefix(a, "--commission-workers") || strings.HasPrefix(a, "-commission-validators") || strings.HasPrefix(a, "-commission-workers") {
			return fmt.Errorf("native queue overrides forbidden")
		}
	}
	f, err := os.Open(binary)
	if err != nil {
		return fmt.Errorf("cannot read pinned native executable")
	}
	hash := sha256.New()
	_, copyErr := io.Copy(hash, f)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil || hex.EncodeToString(hash.Sum(nil)) != digest {
		return fmt.Errorf("native executable digest mismatch")
	}
	info, err := os.Lstat(output)
	if err != nil || !filepath.IsAbs(output) || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("invalid candidate attempt root")
	}
	dir, err := os.MkdirTemp(output, "native-attempt-")
	if err != nil {
		return err
	}
	capture := filepath.Join(dir, "captures")
	if err := os.Mkdir(capture, 0700); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(dir, "native.log"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	argv := append(append([]string{}, args...), "--commission-workers=1", "--commission-validators="+root)
	application := os.Getenv("PGAPPNAME") + "-a" + privateSHA([]byte(dir))[:8]
	if os.Getenv("PGAPPNAME") == "" || len(application) > 63 || cancelNative == nil {
		return fmt.Errorf("unique owned native cancellation is required")
	}
	cmd := exec.CommandContext(ctx, binary, argv...)
	cmd.Cancel = func() error {
		cancelErr := cancelNative(application)
		killErr := cmd.Process.Kill()
		if cancelErr != nil {
			return cancelErr
		}
		return killErr
	}
	cmd.Env = []string{}
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "COSMOS_TAX_COMMISSION_CAPTURE_DIR=") && !strings.HasPrefix(e, "PGOPTIONS=") && !strings.HasPrefix(e, "PGAPPNAME=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "COSMOS_TAX_COMMISSION_CAPTURE_DIR="+capture, "PGAPPNAME="+application, "PGOPTIONS=-c max_parallel_workers_per_gather=0 -c work_mem=32MB -c lock_timeout=1000 -c statement_timeout=60000 -c client_connection_check_interval=1000")
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pinned native attempt could not start")
	}
	birth := "unavailable"
	if stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid)); err == nil {
		if end := strings.LastIndexByte(string(stat), ')'); end >= 0 {
			fields := strings.Fields(string(stat)[end+1:])
			if len(fields) > 19 {
				birth = fields[19]
			}
		}
	}
	identity := fmt.Sprintf("pid=%d\nbirth_ticks=%s\nstarted_utc=%s\nroot_sha256=%s\nbinary_sha256=%s\napplication=%s\n", cmd.Process.Pid, birth, time.Now().UTC().Format(time.RFC3339Nano), privateSHA([]byte(root)), digest, application)
	if err := os.WriteFile(filepath.Join(dir, "native.identity"), []byte(identity), 0600); err != nil {
		_ = cmd.Cancel()
		_ = cmd.Wait()
		return err
	}
	waitErr := cmd.Wait()
	exitCode := cmd.ProcessState.ExitCode()
	if err := os.WriteFile(filepath.Join(dir, "native.exit"), []byte(fmt.Sprintf("%d\n", exitCode)), 0600); err != nil {
		return err
	}
	if waitErr != nil {
		return fmt.Errorf("pinned native attempt failed or yielded")
	}
	return log.Sync()
}
