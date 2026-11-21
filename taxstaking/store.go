package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
	"gorm.io/gorm"
)

var errStateMoved = errors.New("state moved: another writer extended the tables")

// writeTx runs fn in a transaction that holds the staking writer lock.
func writeTx(db *gorm.DB, fn func(tx *gorm.DB) error) error {
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET LOCAL lock_timeout = '10s'`).Error; err != nil {
			return err
		}
		if err := tx.Exec(`SET LOCAL statement_timeout = '600s'`).Error; err != nil {
			return err
		}
		if err := tx.Exec(`SELECT pg_advisory_xact_lock(?)`, taxapi.StakingLockKey).Error; err != nil {
			return err
		}
		return fn(tx)
	})
}

func insertDays(tx *gorm.DB, chain string, days []taxapi.StakingDayRow) error {
	const per = 500
	for i := 0; i < len(days); i += per {
		j := min(i+per, len(days))
		ph := make([]string, 0, j-i)
		args := make([]any, 0, (j-i)*19)
		for _, d := range days[i:j] {
			ph = append(ph, "(?, ?::date, ?, ?, ?, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::numeric, ?::jsonb)")
			args = append(args, chain, d.Day, d.FirstHeight, d.LastHeight, d.Blocks, d.Minted, d.CommunityPool, d.Stakers, d.Commission,
				d.RewardsEvents, d.CommissionEvents, d.SupplyStart, d.InflationStart, d.InflationAvg, d.BondedAvg, d.CommunityTax,
				d.WithdrawnCommission, d.WithdrawnRewards, d.Proposals)
		}
		if err := tx.Exec(`INSERT INTO tax_staking_days (chain, day, first_height, last_height, blocks, minted_uatom,
 community_pool_uatom, stakers_uatom, commission_uatom, rewards_events_uatom, commission_events_uatom, supply_start_uatom,
 inflation_param_start, inflation_param_avg, bonded_ratio_avg, community_tax_start, withdrawn_commission_uatom,
 withdrawn_rewards_uatom, proposals) VALUES `+strings.Join(ph, ","), args...).Error; err != nil {
			return fmt.Errorf("insert days: %v", err)
		}
	}
	return nil
}

func insertValidators(tx *gorm.DB, chain string, vals []taxapi.StakingValidatorRow) error {
	const per = 5000
	for i := 0; i < len(vals); i += per {
		j := min(i+per, len(vals))
		ph := make([]string, 0, j-i)
		args := make([]any, 0, (j-i)*5)
		for _, v := range vals[i:j] {
			ph = append(ph, "(?, ?::date, ?, ?::numeric, ?::numeric)")
			args = append(args, chain, v.Day, v.Operator, v.Rewards, v.Commission)
		}
		if err := tx.Exec(`INSERT INTO tax_staking_validator_days (chain, day, operator, rewards_uatom, commission_uatom) VALUES `+
			strings.Join(ph, ","), args...).Error; err != nil {
			return fmt.Errorf("insert validator days: %v", err)
		}
	}
	return nil
}

type unbondDayRow struct {
	Day string
	unbondRow
}

func insertUnbonding(tx *gorm.DB, chain string, rows []unbondDayRow) error {
	const per = 5000
	for i := 0; i < len(rows); i += per {
		j := min(i+per, len(rows))
		ph := make([]string, 0, j-i)
		args := make([]any, 0, (j-i)*4)
		for _, u := range rows[i:j] {
			ph = append(ph, "(?, ?::date, ?::date, ?::numeric)")
			args = append(args, chain, u.Day, u.CompletionDay, u.Amount)
		}
		if err := tx.Exec(`INSERT INTO tax_staking_unbonding (chain, day, completion_day, amount_uatom) VALUES `+
			strings.Join(ph, ","), args...).Error; err != nil {
			return fmt.Errorf("insert unbonding: %v", err)
		}
	}
	return nil
}

// appendDay writes one computed day and moves the state past it, only if
// the state still ends where this run read it.
func appendDay(db *gorm.DB, chain string, prev taxapi.StakingState, c computedDay) error {
	return writeTx(db, func(tx *gorm.DB) error {
		var lastHeight int64
		var lastDay string
		row := tx.Raw(`SELECT last_height, last_day::text FROM tax_staking_state WHERE chain = ? FOR UPDATE`, chain).Row()
		if err := row.Scan(&lastHeight, &lastDay); err != nil {
			return fmt.Errorf("read state: %v", err)
		}
		if lastHeight != prev.LastHeight || lastDay != prev.LastDay {
			return errStateMoved
		}
		if c.Range.First != prev.LastHeight+1 {
			return fmt.Errorf("day %s starts at %d, state ends at %d", c.Range.Day, c.Range.First, prev.LastHeight)
		}
		if err := insertDays(tx, chain, []taxapi.StakingDayRow{c.Row}); err != nil {
			return err
		}
		if err := insertValidators(tx, chain, c.Vals); err != nil {
			return err
		}
		var ub []unbondDayRow
		for _, u := range c.Unbond {
			ub = append(ub, unbondDayRow{Day: c.Range.Day, unbondRow: u})
		}
		if err := insertUnbonding(tx, chain, ub); err != nil {
			return err
		}
		return tx.Exec(`UPDATE tax_staking_state SET last_day = ?::date, last_height = ?, updated_at = ?, source = 'refresh'
 WHERE chain = ?`, c.Range.Day, c.Range.Last, time.Now().UTC(), chain).Error
	})
}

// readState reads the state row in a read-only transaction.
func readState(db *gorm.DB, chain string) (taxapi.StakingState, bool, error) {
	var st taxapi.StakingState
	var ok bool
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`SET TRANSACTION READ ONLY`).Error; err != nil {
			return err
		}
		var err error
		st, ok, err = taxapi.ReadStakingState(tx, chain)
		return err
	})
	return st, ok, err
}
