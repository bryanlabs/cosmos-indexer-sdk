package rpc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/config"
	abci "github.com/cometbft/cometbft/abci/types"
	tmjson "github.com/cometbft/cometbft/libs/json"
	cmtproto "github.com/cometbft/cometbft/proto/tendermint/types"
	jsonrpc "github.com/cometbft/cometbft/rpc/jsonrpc/client"
	types "github.com/cometbft/cometbft/rpc/jsonrpc/types"
)

func argsToURLValues(args map[string]interface{}) (url.Values, error) {
	values := make(url.Values)
	if len(args) == 0 {
		return values, nil
	}

	err := argsToJSON(args)
	if err != nil {
		return nil, err
	}

	for key, val := range args {
		values.Set(key, val.(string))
	}

	return values, nil
}

func argsToJSON(args map[string]interface{}) error {
	for k, v := range args {
		rt := reflect.TypeOf(v)
		isByteSlice := rt.Kind() == reflect.Slice && rt.Elem().Kind() == reflect.Uint8
		if isByteSlice {
			bytes := reflect.ValueOf(v).Bytes()
			args[k] = fmt.Sprintf("0x%X", bytes)
			continue
		}

		data, err := tmjson.Marshal(v)
		if err != nil {
			return err
		}
		args[k] = string(data)
	}
	return nil
}

func (c *URIClient) DoHTTPGet(ctx context.Context, method string, params map[string]interface{}, result interface{}) (interface{}, error) {
	values, err := argsToURLValues(params)
	if err != nil {
		return nil, fmt.Errorf("failed to encode params: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.Address+"/"+method, nil)
	if err != nil {
		return nil, fmt.Errorf("error creating new request: %w", err)
	}

	req.URL.RawQuery = values.Encode()
	// fmt.Printf("Query string: %s\n", values.Encode())

	// req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if c.AuthHeader != "" {
		req.Header.Add("Authorization", c.AuthHeader)
	}

	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	defer resp.Body.Close()

	responseBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	return unmarshalResponseBytes(responseBytes, jsonrpc.URIClientRequestID, result)
}

// DoHTTPPost supports gateways that expose JSON-RPC at their exact base URL
// but do not route URI-style /method requests.
func (c *URIClient) DoHTTPPost(ctx context.Context, method string, params map[string]interface{}, result interface{}) (interface{}, error) {
	request, err := types.MapToRequest(jsonrpc.URIClientRequestID, method, params)
	if err != nil {
		return nil, fmt.Errorf("failed to encode RPC request: %w", err)
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal RPC request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Address, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create RPC request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.AuthHeader != "" {
		req.Header.Set("Authorization", c.AuthHeader)
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("RPC POST: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("RPC POST returned HTTP %d", resp.StatusCode)
	}
	response, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024*1024+1))
	if err != nil {
		return nil, fmt.Errorf("read RPC response: %w", err)
	}
	if len(response) > 64*1024*1024 {
		return nil, errors.New("RPC response exceeds 64 MiB limit")
	}
	return unmarshalResponseBytes(response, jsonrpc.URIClientRequestID, result)
}

type URIClient struct {
	Address    string
	Client     *http.Client
	AuthHeader string
	JSONRPC    bool
}

func unmarshalResponseBytes(responseBytes []byte, expectedID types.JSONRPCIntID, result interface{}) (interface{}, error) {
	// Read response.  If rpc/core/types is imported, the result will unmarshal
	// into the correct type.
	response := &types.RPCResponse{}
	if err := json.Unmarshal(responseBytes, response); err != nil {
		return nil, fmt.Errorf("error unmarshalling: %w", err)
	}

	if response.Error != nil {
		return nil, response.Error
	}

	if err := validateAndVerifyID(response, expectedID); err != nil {
		return nil, fmt.Errorf("wrong ID: %w", err)
	}

	// Unmarshal the RawMessage into the result.
	if err := tmjson.Unmarshal(response.Result, result); err != nil {
		return nil, fmt.Errorf("error unmarshalling result: %w", err)
	}

	return result, nil
}

func validateAndVerifyID(res *types.RPCResponse, expectedID types.JSONRPCIntID) error {
	if err := validateResponseID(res.ID); err != nil {
		return err
	}
	if expectedID != res.ID.(types.JSONRPCIntID) { // validateResponseID ensured res.ID has the right type
		return fmt.Errorf("response ID (%d) does not match request ID (%d)", res.ID, expectedID)
	}
	return nil
}

func validateResponseID(id interface{}) error {
	if id == nil {
		return errors.New("no ID")
	}
	_, ok := id.(types.JSONRPCIntID)
	if !ok {
		return fmt.Errorf("expected JSONRPCIntID, but got: %T", id)
	}
	return nil
}

// This type **should** cover SDK v0.4x and v0.50, but updates will need to be monitored
type CustomBlockResults struct {
	Height                int64                     `json:"height"`
	TxsResults            []*abci.ResponseDeliverTx `json:"txs_results"`
	BeginBlockEvents      []abci.Event              `json:"begin_block_events"`
	EndBlockEvents        []abci.Event              `json:"end_block_events"`
	ValidatorUpdates      []abci.ValidatorUpdate    `json:"validator_updates"`
	ConsensusParamUpdates *cmtproto.ConsensusParams `json:"consensus_param_updates"`
	FinalizeBlockEvents   []abci.Event              `json:"finalize_block_events"`
}

func (c *URIClient) DoBlockResults(ctx context.Context, height *int64) (*CustomBlockResults, error) {
	result := new(CustomBlockResults)
	params := make(map[string]interface{})
	if height != nil {
		params["height"] = height
	}

	var err error
	if c.JSONRPC {
		_, err = c.DoHTTPPost(ctx, "block_results", params, result)
	} else {
		_, err = c.DoHTTPGet(ctx, "block_results", params, result)
	}
	if err != nil {
		return nil, err
	}

	return result, nil
}

func GetBlockResult(client URIClient, height int64) (*CustomBlockResults, error) {
	brctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()

	bresults, err := client.DoBlockResults(brctx, &height)
	if err != nil {
		return nil, err
	}

	return bresults, nil
}

func GetBlockResultWithRetry(client URIClient, height int64, retryMaxAttempts int64, retryMaxWaitSeconds uint64) (*CustomBlockResults, error) {
	if retryMaxAttempts == 0 {
		return GetBlockResult(client, height)
	}

	if retryMaxWaitSeconds < 2 {
		retryMaxWaitSeconds = 2
	}

	var attempts int64
	maxRetryTime := time.Duration(retryMaxWaitSeconds) * time.Second
	if maxRetryTime < 0 {
		config.Log.Warn("Detected maxRetryTime overflow, setting time to sane maximum of 30s")
		maxRetryTime = 30 * time.Second
	}

	currentBackoffDuration, maxReached := GetBackoffDurationForAttempts(attempts, maxRetryTime)

	for {
		resp, err := GetBlockResult(client, height)
		attempts++
		if err != nil && (retryMaxAttempts < 0 || (attempts <= retryMaxAttempts)) {
			config.Log.Error("Error getting RPC response, backing off and trying again", err)
			config.Log.Debugf("Attempt %d with wait time %+v", attempts, currentBackoffDuration)
			time.Sleep(currentBackoffDuration)

			// guard against overflow
			if !maxReached {
				currentBackoffDuration, maxReached = GetBackoffDurationForAttempts(attempts, maxRetryTime)
			}

		} else {
			if err != nil {
				config.Log.Error("Error getting RPC response, reached max retry attempts")
			}
			return resp, err
		}
	}
}

func GetBackoffDurationForAttempts(numAttempts int64, maxRetryTime time.Duration) (time.Duration, bool) {
	backoffBase := 1.5
	backoffDuration := time.Duration(math.Pow(backoffBase, float64(numAttempts)) * float64(time.Second))

	maxReached := false
	if backoffDuration > maxRetryTime || backoffDuration < 0 {
		maxReached = true
		backoffDuration = maxRetryTime
	}

	return backoffDuration, maxReached
}
