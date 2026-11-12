package sequence

// Ledger follows every account's sequence number through a hub one
// transaction at a time, using the signatures.
//
// A signature covers the sequence number its signer used, and the ante
// handler accepts a transaction only when that number equals the account's
// sequence at that moment. So a transaction that paid its fee and verifies at
// sequence s proves the account stood at s before it and at s+1 after it. A
// failure whose fee the block results cannot prove sits between two such
// points, and the distance between them says how many of the failures in
// between paid. Only a signature that verifies is used as evidence. One that
// does not verify proves nothing and leaves its transaction unsettled.
type Ledger struct {
	opening, closing *Genesis
	accounts         map[string]*ledgerAccount
	verdicts         map[string]map[string]bool
	epoch            int
	// created is the order in which addresses outside the opening file first
	// received coins, and offset the last difference between that order and
	// an account number a signature proved. They are used only when the
	// closing file does not record account numbers.
	created map[string]uint64
	offset  int64
	guess   bool
	// Signatures counts signatures by fee charge and outcome.
	Signatures map[string]int64
	// Unverified counts checked signatures that did not verify, by kind.
	Unverified map[string]int64
	// Impossible counts accounts whose signed sequences contradict the fee
	// charges recorded for their transactions.
	Impossible int
}

// GapWindow is how far past the known sequence a signature is searched for
// after heights were missing. NumberWindow is how far either side of its
// estimate an unrecorded account number is searched for.
const (
	GapWindow    = 20000
	NumberWindow = 4000
)

type pendingFailure struct {
	key    string
	signed int64
}

type ledgerAccount struct {
	seq      uint64
	slack    uint64
	number   uint64
	numbered bool
	epoch    int
	pending  []pendingFailure
}

// Signed is one signer of a transaction. Try reports whether the signer's
// signature verifies for an account number and sequence. A nil Try means the
// signature cannot be checked.
type Signed struct {
	Signer string
	Try    func(account, sequence uint64) bool
	// Kind describes the transaction without identifying it. Signatures that
	// were checked and did not verify are counted by it.
	Kind string
}

func NewLedger(opening, closing *Genesis) *Ledger {
	l := &Ledger{opening: opening, closing: closing, accounts: map[string]*ledgerAccount{}, verdicts: map[string]map[string]bool{}, Signatures: map[string]int64{}, Unverified: map[string]int64{}, created: map[string]uint64{}}
	l.guess = true
	for _, number := range closing.Number {
		if number != 0 {
			l.guess = false
			break
		}
	}
	return l
}

// Received records, in chain order, an address that was sent coins by a
// successful transaction. An address outside the opening file became an
// account, with the next account number, the first time that happened.
func (l *Ledger) Received(address string) {
	if _, known := l.opening.Sequence[address]; known {
		return
	}
	if _, seen := l.created[address]; !seen {
		l.created[address] = uint64(len(l.created))
	}
}

// Gap records that heights were missing, so every account may have moved by
// an unknown amount. Nothing after a gap is settled.
func (l *Ledger) Gap() { l.epoch++ }

func (l *Ledger) note(key, verdict string) {
	if l.verdicts[key] == nil {
		l.verdicts[key] = map[string]bool{}
	}
	l.verdicts[key][verdict] = true
}

func (l *Ledger) account(signer string) *ledgerAccount {
	a := l.accounts[signer]
	if a == nil {
		a = &ledgerAccount{seq: l.opening.Sequence[signer] + l.opening.GenesisSigned[signer]}
		l.accounts[signer] = a
	}
	if a.epoch != l.epoch {
		a.epoch = l.epoch
		l.abandon(a)
		a.slack = GapWindow
	}
	return a
}

func (l *Ledger) abandon(a *ledgerAccount) {
	for _, p := range a.pending {
		l.note(p.key, "unsettled")
	}
	a.slack += uint64(len(a.pending))
	a.pending = nil
}

func (l *Ledger) numbers(signer string, a *ledgerAccount) []uint64 {
	if a.numbered {
		return []uint64{a.number}
	}
	var out []uint64
	add := func(set map[string]uint64) {
		n, ok := set[signer]
		for _, seen := range out {
			ok = ok && seen != n
		}
		if ok {
			out = append(out, n)
		}
	}
	if !l.guess {
		add(l.closing.Number)
		add(l.closing.Position)
	}
	add(l.opening.Number)
	add(l.opening.Position)
	return out
}

func (l *Ledger) verify(s Signed, a *ledgerAccount) (uint64, bool) {
	if s.Try == nil {
		return 0, false
	}
	last := a.seq + a.slack + uint64(len(a.pending))
	if closed, ok := l.closing.Sequence[s.Signer]; ok && a.slack > 0 && closed < last {
		last = closed
	}
	numbers := l.numbers(s.Signer, a)
	for seq := a.seq; seq <= last; seq++ {
		for _, number := range numbers {
			if s.Try(number, seq) {
				a.number, a.numbered = number, true
				return seq, true
			}
		}
	}
	if _, genesis := l.opening.Sequence[s.Signer]; genesis || a.numbered || !l.guess {
		return 0, false
	}
	// The account was created during the hub and no file records its number.
	// Numbers were handed out in creation order, so search outward from the
	// place of the address in that order.
	index, seen := l.created[s.Signer]
	if !seen {
		index = uint64(len(l.created))
	}
	base := int64(len(l.opening.Sequence)) + int64(index)
	for d := int64(0); d <= NumberWindow; d++ {
		for _, number := range []int64{base + l.offset + d, base + l.offset - d} {
			if number < 0 || (d == 0 && number != base+l.offset+d) {
				continue
			}
			for seq := a.seq; seq <= last; seq++ {
				if s.Try(uint64(number), seq) {
					a.number, a.numbered = uint64(number), true
					l.offset = number - base
					return seq, true
				}
			}
		}
	}
	return 0, false
}

// Add records one transaction, in chain order. Charge is "charged", "not
// charged" or "unknown".
func (l *Ledger) Add(key, charge string, signers []Signed) {
	for _, s := range signers {
		a := l.account(s.Signer)
		seq, ok := l.verify(s, a)
		outcome := ", signature verified"
		if s.Try == nil {
			outcome = ", signature not checkable"
		} else if !ok {
			outcome = ", signature not verified"
			origin := "account created during the hub"
			if _, genesis := l.opening.Sequence[s.Signer]; genesis {
				origin = "genesis account"
			}
			l.Unverified["fee "+charge+", "+origin+", "+s.Kind]++
		}
		l.Signatures["fee "+charge+outcome]++
		switch charge {
		case "charged":
			if !ok {
				l.abandon(a)
				a.seq++
				continue
			}
			if a.slack == 0 {
				l.settle(a, seq)
			} else {
				l.abandon(a)
			}
			a.seq, a.slack = seq+1, 0
		case "unknown":
			signed := int64(-1)
			if ok {
				signed = int64(seq)
			}
			a.pending = append(a.pending, pendingFailure{key: key, signed: signed})
		}
	}
}

// settle decides the pending failures of an account that stood at a.seq
// before them and at target after them. A failure can have paid only if its
// signature verifies for the sequence the account had reached, or was never
// verified at all. Each failure gets the verdict shared by every way of
// reaching the target, if there is one.
func (l *Ledger) settle(a *ledgerAccount, target uint64) {
	pending := a.pending
	a.pending = nil
	if len(pending) == 0 {
		return
	}
	n := len(pending)
	// reach[i][k] says the target is reachable from item i with k paid so far.
	reach := make([][]bool, n+1)
	for i := range reach {
		reach[i] = make([]bool, n+2)
	}
	for k := 0; k <= n; k++ {
		reach[n][k] = a.seq+uint64(k) == target
	}
	canPay := func(i, k int) bool {
		return pending[i].signed < 0 || uint64(pending[i].signed) == a.seq+uint64(k)
	}
	for i := n - 1; i >= 0; i-- {
		for k := 0; k <= i; k++ {
			reach[i][k] = reach[i+1][k] || (canPay(i, k) && reach[i+1][k+1])
		}
	}
	if !reach[0][0] {
		l.Impossible++
		for _, p := range pending {
			l.note(p.key, "unsettled")
		}
		return
	}
	live := make([]bool, n+2)
	live[0] = true
	for i := 0; i < n; i++ {
		next := make([]bool, n+2)
		paid, unpaid := false, false
		for k := 0; k <= i; k++ {
			if !live[k] {
				continue
			}
			if reach[i+1][k] {
				unpaid, next[k] = true, true
			}
			if canPay(i, k) && reach[i+1][k+1] {
				paid, next[k+1] = true, true
			}
		}
		switch {
		case paid && !unpaid:
			l.note(pending[i].key, "charged")
		case unpaid && !paid:
			l.note(pending[i].key, "not charged")
		default:
			l.note(pending[i].key, "unsettled")
		}
		live = next
	}
}

// LedgerReport is the outcome of following a hub. Every field is a count
// except Resolutions, which is keyed by chain, height and index.
type LedgerReport struct {
	// Accounts is every account in either genesis file or the ledger.
	Accounts int
	// Exact accounts end on the sequence the closing file records.
	Exact int
	// Mismatched accounts end on another sequence: a fee paying transaction
	// of theirs is missing from the index or counted twice.
	Mismatched int
	// Loose accounts hold a fee paying transaction whose signature did not
	// verify, so their end sequence cannot be compared.
	Loose int
	// Impossible accounts have signed sequences that contradict the recorded
	// fee charges.
	Impossible int
	// Conflicts counts failures two signers settled differently.
	Conflicts                                    int
	SettledCharged, SettledNotCharged, Unsettled int
	Resolutions                                  map[string]string
}

// Close ends the ledger. With whole set, every height of the hub was added
// with no gap, so each account must end on its closing sequence and the
// failures still pending are settled against it.
func (l *Ledger) Close(whole bool) LedgerReport {
	report := LedgerReport{Resolutions: map[string]string{}}
	names := map[string]bool{}
	for _, set := range []map[string]uint64{l.opening.Sequence, l.opening.GenesisSigned, l.closing.Sequence} {
		for name := range set {
			names[name] = true
		}
	}
	for name := range l.accounts {
		names[name] = true
	}
	report.Accounts = len(names)
	whole = whole && l.epoch == 0
	for name := range names {
		a := l.accounts[name]
		if a == nil {
			a = &ledgerAccount{seq: l.opening.Sequence[name] + l.opening.GenesisSigned[name]}
		}
		if !whole || a.slack > 0 {
			l.abandon(a)
			if whole {
				report.Loose++
			}
			continue
		}
		target := l.closing.Sequence[name]
		if len(a.pending) == 0 {
			if a.seq == target {
				report.Exact++
			} else {
				report.Mismatched++
			}
			continue
		}
		before := l.Impossible
		l.settle(a, target)
		if l.Impossible == before {
			report.Exact++
		}
	}
	report.Impossible = l.Impossible
	for key, seen := range l.verdicts {
		switch {
		case seen["charged"] && seen["not charged"]:
			report.Conflicts++
			report.Unsettled++
		case seen["charged"]:
			report.SettledCharged++
			report.Resolutions[key] = "charged"
		case seen["not charged"]:
			report.SettledNotCharged++
			report.Resolutions[key] = "not charged"
		default:
			report.Unsettled++
		}
	}
	return report
}

// SortedResolutions returns the settled inclusion keys in a stable order.
func (r LedgerReport) SortedResolutions() []string {
	return Report{Resolutions: r.Resolutions}.SortedResolutions()
}
