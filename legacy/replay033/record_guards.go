package main

import (
	"fmt"
	abci "github.com/tendermint/tendermint/abci/types"
	"path/filepath"
)

func createRecordingAttempt(home string) error {
	f, err := createPrivateEvidence(filepath.Join(home, ".native033-recording-attempt"))
	if err != nil {
		return fmt.Errorf("recording home was already attempted; use a fresh independent checkpoint clone")
	}
	defer f.Close()
	if _, err = f.WriteString("single-attempt native033 recording home\n"); err != nil {
		return fmt.Errorf("cannot persist recording attempt")
	}
	if f.Sync() != nil {
		return fmt.Errorf("cannot sync recording attempt")
	}
	return f.Close()
}
func requireRecordedResults(c *cachedResults, begin abci.ResponseBeginBlock, deliver []*abci.ResponseDeliverTx, end abci.ResponseEndBlock) error {
	if c == nil {
		return fmt.Errorf("recorded height lacks full native cached results")
	}
	if what := differ(c, begin, deliver, end); what != "" {
		return fmt.Errorf("recorded native results differ from cache: %s", what)
	}
	return nil
}
