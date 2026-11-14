package taxapi

import (
	"encoding/json"
	"fmt"
	"io"
	"time"
)

// ExportCommissionFlowVersions is the explicit offline-audit export path for
// both legacy and chunked private results. All JSON strings are reassembled and
// verified before exporting that validator. Keep output private and unpublished
// until this method succeeds; an error may leave a partial array in out.
// Membership is explicit, never silently inferred from the available subset.
// Status is deliberately retained, including failures, for the audit to assess.
// This does not migrate, write the DB, or promote any result for serving.
func (s *Server) ExportCommissionFlowVersions(o CommissionFlowBuildOptions, chain string, from time.Time, validators []string, out io.Writer) error {
	if err := o.validate(); err != nil {
		return err
	}
	if !from.Before(o.AsOf) || len(validators) == 0 {
		return fmt.Errorf("invalid private export window or membership")
	}
	seen := map[string]bool{}
	for _, validator := range validators {
		if validator == "" || seen[validator] {
			return fmt.Errorf("private export identities must be nonempty and unique")
		}
		seen[validator] = true
	}
	if _, err := io.WriteString(out, "[\n"); err != nil {
		return err
	}
	for i, validator := range validators {
		snapshot, err := s.LoadCommissionFlowVersion(o.Version, chain, validator)
		if err != nil {
			return err
		}
		var run CommissionFlowRun
		if err := json.Unmarshal([]byte(snapshot.RunJSON), &run); err != nil {
			return fmt.Errorf("invalid private result run")
		}
		if snapshot.PrivateProfileSHA256 != o.profileSHA() || snapshot.Build != o.Build || snapshot.Version != o.Version || snapshot.Chain != chain || snapshot.Validator != validator || run.Chain != chain || run.Validator != validator || !run.FromDay.Equal(from) || !run.AsOf.Equal(o.AsOf.AddDate(0, 0, -1)) {
			return fmt.Errorf("private export result has different provenance or window")
		}
		if i > 0 {
			if _, err := io.WriteString(out, ",\n"); err != nil {
				return err
			}
		}
		// Strings intentionally preserve exact JSON bytes after the outer
		// export is decoded. The existing offline audit already decodes them.
		row := struct {
			Version              string    `json:"version"`
			Chain                string    `json:"chain"`
			Validator            string    `json:"validator"`
			Build                string    `json:"build"`
			PrivateProfileSHA256 string    `json:"private_profile_sha256,omitempty"`
			ComputedAt           time.Time `json:"computed_at"`
			RunJSON              string    `json:"run_json"`
			RowsJSON             string    `json:"rows_json"`
			EvidenceJSON         string    `json:"evidence_json"`
		}{snapshot.Version, snapshot.Chain, snapshot.Validator, snapshot.Build, snapshot.PrivateProfileSHA256, snapshot.ComputedAt, snapshot.RunJSON, snapshot.RowsJSON, snapshot.EvidenceJSON}
		if err := json.NewEncoder(out).Encode(row); err != nil {
			return err
		}
	}
	_, err := io.WriteString(out, "]\n")
	return err
}
