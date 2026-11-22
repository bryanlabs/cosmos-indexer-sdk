package main

// Exact arithmetic that reproduces the extraction scripts (merge.py,
// export_v.py), which use Python Decimal with a 60 digit context: sums of
// chain amounts are exact, a division or product is rounded to 60
// significant digits half-even, and a result is then rounded to a whole
// number half-even (or truncated where the scripts call int()).

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/DefiantLabs/cosmos-indexer/taxapi"
)

const decimalPrec = 60

var (
	big10  = big.NewInt(10)
	fixOne = big.NewInt(1_000_000_000_000_000_000)
)

func parseFix(s string) (*big.Int, error) { return taxapi.ParseFix18(s) }

func fixString(v *big.Int) string { return taxapi.Fix18String(v) }

func fixRat(v *big.Int) *big.Rat { return new(big.Rat).SetFrac(v, fixOne) }

// fixTrunc is Python's int() of a Decimal: toward zero.
func fixTrunc(v *big.Int) *big.Int { return new(big.Int).Quo(v, fixOne) }

func pow10(n int) *big.Int { return new(big.Int).Exp(big10, big.NewInt(int64(n)), nil) }

// roundHalfEven rounds q to a whole number, ties to even.
func roundHalfEven(q *big.Rat) *big.Int {
	num, den := q.Num(), q.Denom()
	quo, rem := new(big.Int).QuoRem(num, den, new(big.Int))
	r2 := new(big.Int).Abs(rem)
	r2.Lsh(r2, 1)
	c := r2.Cmp(den)
	if c > 0 || (c == 0 && quo.Bit(0) == 1) {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	return quo
}

// roundSig rounds q to digits significant digits, ties to even, as a Decimal
// context of that precision does after a division or product.
func roundSig(q *big.Rat, digits int) *big.Rat {
	if q.Sign() == 0 {
		return new(big.Rat)
	}
	a := new(big.Rat).Abs(q)
	// e with 10^e <= a < 10^(e+1)
	e := len(a.Num().String()) - len(a.Denom().String())
	ten := func(n int) *big.Rat {
		if n >= 0 {
			return new(big.Rat).SetInt(pow10(n))
		}
		return new(big.Rat).SetFrac(big.NewInt(1), pow10(-n))
	}
	for ten(e).Cmp(a) > 0 {
		e--
	}
	for ten(e+1).Cmp(a) <= 0 {
		e++
	}
	s := digits - 1 - e
	scaled := new(big.Rat).Mul(a, ten(s))
	n := roundHalfEven(scaled)
	out := new(big.Rat).Mul(new(big.Rat).SetInt(n), ten(-s))
	if q.Sign() < 0 {
		out.Neg(out)
	}
	return out
}

func decDiv(a, b *big.Rat) *big.Rat { return roundSig(new(big.Rat).Quo(a, b), decimalPrec) }
func decMul(a, b *big.Rat) *big.Rat { return roundSig(new(big.Rat).Mul(a, b), decimalPrec) }

// meanPlaces is sum/n rounded half-even to places decimals, written the way
// Python writes round(x, places): shortest form, "0.1" not "0.10000000".
func meanPlaces(sum *big.Int, n int64, places int) string {
	// sum is 1e18-scaled; the mean times 10^places is sum*10^places/(1e18*n).
	q := new(big.Rat).SetFrac(new(big.Int).Mul(sum, pow10(places)), new(big.Int).Mul(fixOne, big.NewInt(n)))
	v := roundHalfEven(q)
	return fixString(new(big.Int).Mul(v, pow10(18-places)))
}

// taxString writes a community tax the way the scripts' constants read:
// at least two decimals ("0.02", "0.10").
func taxString(v *big.Int) string {
	s := fixString(v)
	ip, fp, _ := strings.Cut(s, ".")
	for len(fp) < 2 {
		fp += "0"
	}
	return ip + "." + fp
}

// legacyDec reads a cosmos LegacyDec integer string (value times 1e18).
func legacyDec(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok || v.Sign() < 0 {
		return nil, fmt.Errorf("bad LegacyDec %q", s)
	}
	return v, nil
}
