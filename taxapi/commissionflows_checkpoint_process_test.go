package taxapi

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestFlowCheckpointTimezoneIdentity(t *testing.T) {
	for _, zone := range []*time.Location{time.Local, time.FixedZone("fixture-offset", -7*3600)} {
		t.Run(zone.String(), func(t *testing.T) {
			id := checkpointTestID()
			id.From = id.From.In(zone)
			id.AsOf = id.AsOf.In(zone)
			dir := checkpointTestDir(t)
			l, err := openFlowCheckpoint(dir, id, checkpointTestLedger(), checkpointTestHooks())
			require.NoError(t, err)
			_, _, err = l.opening(flowTestReceiver, id.From)
			require.NoError(t, err)
			id.From = id.From.UTC()
			id.AsOf = id.AsOf.UTC()
			resumed, err := openFlowCheckpoint(dir, id, checkpointTestLedger(), checkpointTestHooks())
			require.NoError(t, err)
			_, _, err = resumed.opening(flowTestReceiver, id.From)
			require.NoError(t, err)
			require.NoError(t, resumed.check())
		})
	}
}
func TestFlowCheckpointDeadlineResume(t *testing.T) {
	dir := checkpointTestDir(t)
	base := &checkpointInterruptLoader{flowLoader: checkpointTestLedger(), left: 2, cause: context.DeadlineExceeded}
	l, err := openFlowCheckpoint(dir, checkpointTestID(), base, checkpointTestHooks())
	require.NoError(t, err)
	trace := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, l)
	require.ErrorIs(t, trace.run([]string{flowTestReceiver}), context.DeadlineExceeded)
	require.Error(t, l.check())
	resumed, err := openFlowCheckpoint(dir, checkpointTestID(), checkpointTestLedger(), checkpointTestHooks())
	require.NoError(t, err)
	trace = newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, resumed)
	require.NoError(t, trace.run([]string{flowTestReceiver}))
	require.NoError(t, resumed.check())
	require.Equal(t, checkpointCanonical(t, runFlowTest(t, checkpointTestLedger(), flowTestReceiver)), checkpointCanonical(t, trace))
}
func TestFlowCheckpointSIGTERMHelper(t *testing.T) {
	dir := os.Getenv("FLOW_CHECKPOINT_SIGNAL_HELPER")
	if dir == "" {
		return
	}
	l, err := openFlowCheckpoint(dir, checkpointTestID(), checkpointTestLedger(), checkpointTestHooks())
	require.NoError(t, err)
	_, _, err = l.opening(flowTestReceiver, flowTestStart.AddDate(0, 0, -1))
	require.NoError(t, err)
	_, err = l.events(flowTestReceiver, flowTestStart, checkpointTestID().AsOf)
	require.NoError(t, err)
	fmt.Println("checkpoint-ready")
	time.Sleep(time.Hour)
}
func TestFlowCheckpointActualSIGTERMResume(t *testing.T) {
	dir := checkpointTestDir(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestFlowCheckpointSIGTERMHelper$")
	cmd.Env = append(os.Environ(), "FLOW_CHECKPOINT_SIGNAL_HELPER="+dir, "GOMAXPROCS=2")
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(stdout).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "checkpoint-ready\n", line)
	require.NoError(t, cmd.Process.Signal(syscall.SIGTERM))
	err = cmd.Wait()
	require.Error(t, err)
	exit, ok := err.(*exec.ExitError)
	require.True(t, ok)
	status, ok := exit.Sys().(syscall.WaitStatus)
	require.True(t, ok)
	require.True(t, status.Signaled())
	require.Equal(t, syscall.SIGTERM, status.Signal())
	l, err := openFlowCheckpoint(dir, checkpointTestID(), checkpointTestLedger(), checkpointTestHooks())
	require.NoError(t, err)
	trace := newFlowTrace(flowTestValidator, flowTestStart, checkpointTestID().AsOf, l)
	require.NoError(t, trace.run([]string{flowTestReceiver}))
	require.NoError(t, l.check())
	require.Equal(t, checkpointCanonical(t, runFlowTest(t, checkpointTestLedger(), flowTestReceiver)), checkpointCanonical(t, trace))
}
