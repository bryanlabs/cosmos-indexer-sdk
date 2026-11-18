package balancedays

// PlaceSlashed lets a test name the operator of a consensus address that a
// stored slash names.
func PlaceSlashed(consensus, operator string) {
	slashedOperators[hashOf(consensus)] = hashOf(operator)
}
