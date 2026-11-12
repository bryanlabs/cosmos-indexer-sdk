package config

import (
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestLegacyServingConfigCannotStart(t *testing.T) {
	for _, chain := range []string{"cosmoshub-1", "cosmoshub-2", "cosmoshub-3"} {
		cfg := IndexConfig{}
		cfg.Probe.ChainID = chain
		cfg.Base.BlockEventIndexingEnabled = true
		require.ErrorContains(t, cfg.Validate(), "legacy serving intake blocked")
	}
}

func TestLegacyTerminalConfigOptIn(t *testing.T) {
	cfg := IndexConfig{}
	cmd := &cobra.Command{}
	SetupIndexSpecificFlags(&cfg, cmd)
	require.NoError(t, cmd.ParseFlags([]string{"--base.start-block=9", "--base.end-block=10", "--base.index-transactions", "--base.terminal-archive-height=10"}))
	require.NoError(t, cfg.validateBlockInputValues())
	require.Equal(t, int64(10), cfg.Base.TerminalArchiveHeight)
	require.Empty(t, CheckSuperfluousIndexKeys([]string{"base.terminal-archive-height"}))
	for _, change := range []func(*IndexConfig){func(c *IndexConfig) { c.Base.EndBlock = 9 }, func(c *IndexConfig) { c.Base.TerminalArchiveHeight = -1 }, func(c *IndexConfig) { c.Base.BlockInputFile = "plan.json" }, func(c *IndexConfig) { c.Base.ReindexMessageType = "example" }, func(c *IndexConfig) { c.Base.ReattemptFailedBlocks = true }} {
		bad := cfg
		change(&bad)
		require.Error(t, bad.validateBlockInputValues())
	}
}
