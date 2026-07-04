package taxapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleMethodology(t *testing.T) {
	s := &Server{oracle: NewOracle("http://unused-in-this-test", "")}
	req := httptest.NewRequest(http.MethodGet, "/methodology", nil)
	rec := httptest.NewRecorder()
	s.handleMethodology(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d", rec.Code)
	}
	var m Methodology
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if m.Version == "" || m.EffectiveDate == "" || len(m.Sources) == 0 || len(m.FallbackOrder) == 0 {
		t.Fatalf("methodology should be fully populated: %+v", m)
	}
}

func TestHandlePriceSeriesJSON(t *testing.T) {
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usd":2.5,"found":true}`))
	}))
	defer oracleSrv.Close()

	s := &Server{oracle: NewOracle(oracleSrv.URL, "")}
	req := httptest.NewRequest(http.MethodGet, "/price-series?denom=uatom&start=2026-01-01&end=2026-01-03", nil)
	rec := httptest.NewRecorder()
	s.handlePriceSeries(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Denom              string       `json:"denom"`
		MethodologyVersion string       `json:"methodology_version"`
		Points             []pricePoint `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if body.Denom != "uatom" || body.MethodologyVersion != MethodologyVersion {
		t.Fatalf("wrong metadata: %+v", body)
	}
	if len(body.Points) != 3 {
		t.Fatalf("want 3 daily points (Jan 1-3 inclusive), got %d: %+v", len(body.Points), body.Points)
	}
	if body.Points[0].Date != "2026-01-01" || body.Points[2].Date != "2026-01-03" {
		t.Fatalf("wrong date range: %+v", body.Points)
	}
	for _, p := range body.Points {
		if !p.Found || p.USD != 2.5 {
			t.Fatalf("expected every day priced at 2.5: %+v", p)
		}
	}
}

func TestHandlePriceSeriesUnknownDenomAllUnfound(t *testing.T) {
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usd":0,"found":false}`))
	}))
	defer oracleSrv.Close()

	s := &Server{oracle: NewOracle(oracleSrv.URL, "")}
	req := httptest.NewRequest(http.MethodGet, "/price-series?denom=mystery&start=2026-01-01&end=2026-01-02", nil)
	rec := httptest.NewRecorder()
	s.handlePriceSeries(rec, req)

	var body struct {
		Points []pricePoint `json:"points"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	for _, p := range body.Points {
		if p.Found {
			t.Fatalf("unknown denom should never report found=true: %+v", p)
		}
	}
}

func TestHandlePriceSeriesRangeTooLarge(t *testing.T) {
	s := &Server{oracle: NewOracle("http://unused-in-this-test", "")}
	req := httptest.NewRequest(http.MethodGet, "/price-series?start=2020-01-01&end=2026-01-01", nil)
	rec := httptest.NewRecorder()
	s.handlePriceSeries(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 for an oversized range, got %d", rec.Code)
	}
}

func TestHandlePriceSeriesEndBeforeStart(t *testing.T) {
	s := &Server{oracle: NewOracle("http://unused-in-this-test", "")}
	req := httptest.NewRequest(http.MethodGet, "/price-series?start=2026-01-10&end=2026-01-01", nil)
	rec := httptest.NewRecorder()
	s.handlePriceSeries(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400 when end is before start, got %d", rec.Code)
	}
}

func TestHandlePriceSeriesCSV(t *testing.T) {
	oracleSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usd":1.95,"found":true}`))
	}))
	defer oracleSrv.Close()

	s := &Server{oracle: NewOracle(oracleSrv.URL, "")}
	req := httptest.NewRequest(http.MethodGet, "/price-series?start=2026-01-01&end=2026-01-01&format=csv", nil)
	rec := httptest.NewRecorder()
	s.handlePriceSeries(rec, req)

	if ct := rec.Header().Get("Content-Type"); ct != "text/csv" {
		t.Fatalf("want text/csv, got %q", ct)
	}
	body := rec.Body.String()
	if !strings.HasPrefix(body, "date,denom,usd,found,methodology_version") {
		t.Fatalf("unexpected csv header: %q", body)
	}
	if !strings.Contains(body, "2026-01-01,uatom,1.95,true,"+MethodologyVersion) {
		t.Fatalf("missing expected row: %q", body)
	}
}
