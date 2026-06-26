package taxapi

import (
	"encoding/json"
	"net/http"
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
	mux.HandleFunc("GET /coverage", s.handleCoverage)
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
		case string(tax.CategoryTransfer), string(tax.CategoryIBCOut), string(tax.CategoryNFTSale):
			// Seller (FromAddr) disposes; everyone else (ToAddr) acquires.
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
	m, ok := meta[denom]
	decimals := 6
	symbol := denom
	if ok {
		decimals = m.Decimals
		if m.Symbol != "" {
			symbol = m.Symbol
		}
	}
	amt, err := decimal.NewFromString(amountBase)
	if err != nil {
		amt = decimal.Zero
	}
	amt = amt.Shift(int32(-decimals))

	price := decimal.Zero
	if usdF, found := s.oracle.PriceAt(chain, denom, ts.UTC().Format("2006-01-02")); found {
		price = decimal.NewFromFloat(usdF)
	}
	return Row{
		Time: ts, Category: category, Direction: dir,
		Symbol: symbol, Denom: denom, Amount: amt,
		PriceUSD: price, ValueUSD: amt.Mul(price),
		From: from, To: to, TxHash: hash,
	}
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
