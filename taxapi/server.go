package taxapi

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/DefiantLabs/cosmos-indexer/tax"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

type Server struct {
	db     *gorm.DB
	oracle *Oracle
}

func NewServer(db *gorm.DB, oracle *Oracle) *Server { return &Server{db: db, oracle: oracle} }

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /8949", s.handle8949)
	mux.HandleFunc("GET /schedule-d", s.handleScheduleD)
	mux.HandleFunc("GET /income", s.handleIncome)
	mux.HandleFunc("GET /990t", s.handle990T)
	mux.HandleFunc("GET /coverage", s.handleCoverage)
	mux.HandleFunc("GET /balance", s.handleBalance)
	mux.HandleFunc("GET /price-series", s.handlePriceSeries)
	mux.HandleFunc("GET /methodology", s.handleMethodology)
	return withCORS(mux)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := q.Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	format := def(q.Get("format"), "summ")
	rows, err := s.rowsFor(chain, addr, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=cosmos-tax-"+format+".csv")
	_ = WriteCSV(w, format, rows)
}

func (s *Server) handle8949(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := q.Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	rows, err := s.rowsFor(chain, addr, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=form-8949.csv")
	_ = Write8949CSV(w, Build8949(rows))
}

func (s *Server) handleScheduleD(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := q.Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	rows, err := s.rowsFor(chain, addr, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(BuildScheduleD(Build8949(rows)))
}

// handleIncome returns an ordinary-income report (staking rewards + validator
// commission, valued in USD at receipt) for Schedule 1 / Form 990-T. These are
// income at the time received, distinct from the 8949's capital dispositions.
func (s *Server) handleIncome(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := q.Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	rows, err := s.rowsFor(chain, addr, dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=income-schedule1-990t.csv")
	_, _ = w.Write([]byte(buildIncomeCSV(rows)))
}

// buildIncomeCSV renders the reward/commission rows as the income-report CSV:
// header, one line per row, a TOTAL, and (INF-201) an explicit WARNING line
// when any included row had no known price, since TOTAL then excludes that
// row's value (0, not a real zero) rather than fabricating one.
func buildIncomeCSV(rows []Row) string {
	var buf strings.Builder
	cw := csv.NewWriter(&buf)
	_ = cw.Write([]string{"date_utc", "type", "symbol", "denom", "amount", "unit_price_usd", "value_usd", "tx_hash"})
	total := decimal.Zero
	missingDenoms := map[string]bool{}
	for _, row := range rows {
		if row.Category != "reward" && row.Category != "commission" {
			continue
		}
		total = total.Add(row.ValueUSD)
		if row.PriceMissing {
			missingDenoms[row.Symbol] = true
		}
		_ = cw.Write([]string{
			row.Time.UTC().Format("2006-01-02"), row.Category, row.Symbol, row.Denom,
			row.Amount.String(), row.PriceUSD.String(), row.ValueUSD.StringFixed(2), row.TxHash,
		})
	}
	_ = cw.Write([]string{"", "TOTAL", "", "", "", "", total.StringFixed(2), ""})
	if len(missingDenoms) > 0 {
		denoms := make([]string, 0, len(missingDenoms))
		for sym := range missingDenoms {
			denoms = append(denoms, sym)
		}
		sort.Strings(denoms)
		_ = cw.Write([]string{"", "WARNING", "", "", "", "",
			fmt.Sprintf("no price found for: %s -- TOTAL above excludes their value, actual income is higher", strings.Join(denoms, ", ")),
			"",
		})
	}
	cw.Flush()
	return buf.String()
}

// handle990T computes the Form 990-T / UBIT estimate from an address's staking
// income (for retirement-account wallets): gross UBTI, the $1,000 specific
// deduction, taxable UBTI, and estimated tax at trust rates.
func (s *Server) handle990T(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := q.Get("address")
	if addr == "" {
		http.Error(w, "address required", http.StatusBadRequest)
		return
	}
	chain := def(q.Get("chain"), "mainnet")
	start := dateParam(q.Get("start"), time.Time{})
	end := dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1))
	// An entity may hold several wallets; the $1,000 deduction is per return, so
	// sum staking income across all of them, then compute one 990-T.
	ubti := decimal.Zero
	priceMissingRows := 0
	for _, a := range strings.Split(addr, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		rows, err := s.rowsFor(chain, a, start, end)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		addrUBTI, addrMissing := sumUBTI(rows)
		ubti = ubti.Add(addrUBTI)
		priceMissingRows += addrMissing
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(compute990T(ubti, priceMissingRows))
}

// sumUBTI totals reward/commission ValueUSD across one address's rows for the
// 990-T, and counts how many of those rows had no known price (INF-201) — that
// count feeds compute990T's note, since the total below excludes their value.
func sumUBTI(rows []Row) (ubti decimal.Decimal, priceMissingRows int) {
	ubti = decimal.Zero
	for _, row := range rows {
		if row.Category != "reward" && row.Category != "commission" {
			continue
		}
		ubti = ubti.Add(row.ValueUSD)
		if row.PriceMissing {
			priceMissingRows++
		}
	}
	return ubti, priceMissingRows
}

func (s *Server) handleCoverage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rep, err := s.coverage(dateParam(q.Get("start"), time.Time{}), dateParam(q.Get("end"), nowUTC().AddDate(0, 0, 1)))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rep)
}

// rowsFor loads classified taxable events + fees for an address in [start,end),
// resolves denoms + USD prices, and returns normalized Rows.
func (s *Server) rowsFor(chain, addr string, start, end time.Time) ([]Row, error) {
	meta := s.oracle.Denoms(chain)

	var events []tax.TaxableEvent
	if err := s.db.
		Where("(from_addr = ? OR to_addr = ?)", addr, addr).
		Where("timestamp >= ? AND timestamp < ?", start, end).
		Order("timestamp asc").
		Find(&events).Error; err != nil {
		return nil, err
	}

	out := make([]Row, 0, len(events)+8)
	for _, e := range events {
		dir := "in"
		switch e.Category {
		case string(tax.CategoryTransfer), string(tax.CategoryIBCOut), string(tax.CategoryNFTSale), string(tax.CategorySwap):
			// Disposer (FromAddr) sends; everyone else (ToAddr) acquires.
			if e.FromAddr == addr {
				dir = "out"
			}
		}
		row := s.buildRow(chain, meta, e.Timestamp, e.TxHash, e.Category, dir, e.Denom, e.Amount, e.FromAddr, e.ToAddr)
		row.Asset = e.Asset
		out = append(out, row)
	}

	// Fees (generic SDK Fee table): a spend by the payer.
	type feeRec struct {
		Amount string
		Denom  string
		Ts     time.Time
		Hash   string
	}
	var fees []feeRec
	if err := s.db.Table("fees").
		Select("fees.amount::text AS amount, denoms.base AS denom, blocks.time_stamp AS ts, txes.hash AS hash").
		Joins("JOIN addresses ON addresses.id = fees.payer_address_id").
		Joins("JOIN denoms ON denoms.id = fees.denomination_id").
		Joins("JOIN txes ON txes.id = fees.tx_id").
		Joins("JOIN blocks ON blocks.id = txes.block_id").
		Where("addresses.address = ? AND blocks.time_stamp >= ? AND blocks.time_stamp < ?", addr, start, end).
		Scan(&fees).Error; err == nil {
		for _, f := range fees {
			out = append(out, s.buildRow(chain, meta, f.Ts, f.Hash, "fee", "out", f.Denom, f.Amount, addr, ""))
		}
	}

	return out, nil
}

func (s *Server) buildRow(chain string, meta map[string]DenomMeta, ts time.Time, hash, category, dir, denom, amountBase, from, to string) Row {
	// IBC voucher denoms arrive as trace paths (e.g. "transfer/channel-0/uatom").
	// Resolve to the underlying base asset so symbol, decimals and price match the
	// native token (uatom that round-trips is still ATOM); keep the raw path.
	base, isIBC := ibcBaseDenom(denom)

	// Decimals resolution: the oracle first, then the chain's own bank module,
	// and only if both miss do we assume 6 (and flag it) rather than silently
	// corrupting the amount (INF-200).
	var decimals int
	var symbol string
	assumed := false
	if m, ok := meta[base]; ok {
		decimals, symbol = m.Decimals, m.Symbol
	} else if m, ok := s.oracle.BankMetaFallback(base); ok {
		decimals, symbol = m.Decimals, m.Symbol
	} else {
		decimals, symbol, assumed = 6, base, true
	}
	if symbol == "" {
		symbol = base
	}
	amt, err := decimal.NewFromString(amountBase)
	if err != nil {
		amt = decimal.Zero
	}
	amt = amt.Shift(int32(-decimals))

	price := decimal.Zero
	priceMissing := false
	if usdF, found := s.oracle.PriceAt(chain, base, ts.UTC().Format("2006-01-02")); found {
		price = decimal.NewFromFloat(usdF)
	} else {
		priceMissing = true
	}
	return Row{
		Time: ts, Category: category, Direction: dir,
		Symbol: symbol, Denom: denom, Amount: amt,
		PriceUSD: price, ValueUSD: amt.Mul(price),
		From: from, To: to, TxHash: hash, IsIBC: isIBC,
		DecimalsAssumed: assumed,
		PriceMissing:    priceMissing,
	}
}

// ibcBaseDenom strips leading IBC trace prefixes ("transfer/channel-N/" and
// "<wasm-port>/channel/" pairs) from a voucher denom, returning the underlying
// base denom and whether a prefix was stripped. "transfer/channel-0/uatom" ->
// ("uatom", true); "ibc/HASH" and "uatom" pass through unchanged (false).
func ibcBaseDenom(denom string) (string, bool) {
	parts := strings.Split(denom, "/")
	i := 0
	for i+1 < len(parts) && (parts[i] == "transfer" || strings.HasPrefix(parts[i], "08-wasm-")) {
		i += 2
	}
	if i == 0 {
		return denom, false
	}
	return strings.Join(parts[i:], "/"), true
}

func def(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func dateParam(v string, fallback time.Time) time.Time {
	if v == "" {
		return fallback
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return t
	}
	return fallback
}

func withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}
