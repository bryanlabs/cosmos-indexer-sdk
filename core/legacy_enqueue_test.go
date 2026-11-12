package core

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	"github.com/DefiantLabs/probe/client"
	rpcclient "github.com/cometbft/cometbft/rpc/client"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/stretchr/testify/require"
)

type terminalStatusClient struct {
	rpcclient.Client
	height  int64
	failure error
}

func (c terminalStatusClient) Status(context.Context) (*coretypes.ResultStatus, error) {
	if c.failure != nil {
		return nil, c.failure
	}
	result := new(coretypes.ResultStatus)
	result.SyncInfo.LatestBlockHeight = c.height
	return result, nil
}

func TestLegacyExplicitTerminalEnqueue(t *testing.T) {
	for _, tc := range []struct {
		name             string
		start, end, head int64
		want             []int64
		terminal         bool
	}{
		{"genesis", 1, 1, 1, []int64{1}, true},
		{"middle", 7, 9, 10, []int64{7, 8, 9}, false},
		{"terminal", 9, 10, 10, []int64{9, 10}, true},
		{"hub1-halted-boundary", 500014, 500015, 500015, []int64{500014, 500015}, true},
		{"hub2-halted-boundary", 2902001, 2902002, 2902002, []int64{2902001, 2902002}, true},
		{"hub3-halted-boundary", 5200790, 5200791, 5200791, []int64{5200790, 5200791}, true},
		{"finite-live-head-excluded", 9, 10, 10, []int64{9}, false},
		{"follow-head-excluded", 9, -1, 10, []int64{9}, false},
		{"bounded-unavailable", 9, 11, 10, []int64{9}, false},
		{"already-caught-up", 10, -1, 10, nil, false},
		{"overflow-terminal", math.MaxInt64, math.MaxInt64, math.MaxInt64, []int64{math.MaxInt64}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.IndexConfig{}
			cfg.Base.StartBlock = tc.start
			cfg.Base.EndBlock = tc.end
			cfg.Base.ReIndex = true
			cfg.Base.ExitWhenCaughtUp = true
			cfg.Base.TransactionIndexingEnabled = true
			cfg.Base.BlockEventIndexingEnabled = true
			cl := &client.ChainClient{Config: &client.ChainClientConfig{Timeout: "1s"}, RPCClient: terminalStatusClient{height: tc.head}}
			if tc.terminal {
				cfg.Base.TerminalArchiveHeight = tc.end
			}
			enqueue, err := GenerateDefaultEnqueueFunction(nil, cfg, cl, 1)
			require.NoError(t, err)
			jobs := make(chan *EnqueueData, 16)
			done := make(chan error, 1)
			go func() { done <- enqueue(jobs) }()
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("enqueue did not terminate at the real archive boundary")
			}
			var got []int64
			for len(jobs) > 0 {
				job := <-jobs
				got = append(got, job.Height)
				require.True(t, job.IndexTransactions)
				require.True(t, job.IndexBlockEvents)
			}
			require.Equal(t, tc.want, got)
		})
	}
}

func TestLegacyTerminalDeclarationRequiresMatchingEnd(t *testing.T) {
	cfg := config.IndexConfig{}
	cfg.Base.EndBlock = 10
	for _, terminal := range []int64{-1, 0, 9, 11} {
		enqueue, err := GenerateTerminalArchiveEnqueueFunction(nil, cfg, nil, 1, terminal)
		require.Error(t, err)
		require.Nil(t, enqueue)
	}
}

func TestLegacyTerminalHeadMismatch(t *testing.T) {
	cfg := config.IndexConfig{}
	cfg.Base.StartBlock = 9
	cfg.Base.EndBlock = 10
	cfg.Base.TerminalArchiveHeight = 10
	cfg.Base.ReIndex = true
	for _, head := range []int64{9, 11} {
		cl := &client.ChainClient{Config: &client.ChainClientConfig{Timeout: "1s"}, RPCClient: terminalStatusClient{height: head}}
		enqueue, err := GenerateDefaultEnqueueFunction(nil, cfg, cl, 1)
		require.NoError(t, err)
		jobs := make(chan *EnqueueData, 2)
		require.Error(t, enqueue(jobs))
		require.Empty(t, jobs)
	}
}

func TestLegacyTerminalStatusFailure(t *testing.T) {
	cfg := config.IndexConfig{}
	cfg.Base.StartBlock = 1
	cfg.Base.EndBlock = 1
	cfg.Base.ReIndex = true
	failure := errors.New("status unavailable")
	enqueue, err := GenerateTerminalArchiveEnqueueFunction(nil, cfg, &client.ChainClient{Config: &client.ChainClientConfig{Timeout: "1s"}, RPCClient: terminalStatusClient{failure: failure}}, 1, 1)
	require.NoError(t, err)
	jobs := make(chan *EnqueueData, 1)
	require.ErrorIs(t, enqueue(jobs), failure)
	require.Empty(t, jobs)
}
