package tax

// KnownContract names a CosmWasm contract on Cosmos Hub whose events the
// parser classifies with protocol-specific rules. Most rules key on the wasm
// action and attribute shape rather than the address, so a protocol that
// deploys a new instance keeps working; the registry supplies the name shown
// to users and the kind that disambiguates the few actions whose meaning
// depends on what the contract is (a "claim" from a rewards distributor is
// income, a "claim" from a DAO staking module returns the user's own tokens).
type KnownContract struct {
	Address  string `json:"address"`
	Protocol string `json:"protocol"`
	Name     string `json:"name"`
	// Kind drives classification: marketplace, auction, inscriptions, names,
	// raffle, game, payment, hydro, tribute, vault, rewards, dao_staking,
	// router, bridge, bridge_helper, dex.
	Kind string `json:"kind"`
	// FirstSeenHeight is the first Cosmos Hub block in which this contract
	// emitted events, for the coverage page and for scoped reindexing.
	FirstSeenHeight int64 `json:"first_seen_height"`
}

// KnownContracts is the registry. Heights come from the production index.
var KnownContracts = []KnownContract{
	{"cosmos1clswlqlfm8gpn7n5wu0ypu0ugaj36urlhj7yz30hn7v7mkcm2tuqy9f8s5", "Skip Go", "Skip Go entry point", contractRoleRouter, 29102288},
	{"cosmos1lqu9662kd4my6dww4gzp3730vew0gkwe0nl9ztjh0n5da0a8zc4swsvd22", "Skip Go", "Skip Go IBC callback adapter", contractRoleRouter, 29102288},
	{"cosmos16nff0lmuegn2zr6ca3dp8jnqltpq2c9ayhmqn3avjq0wne95h2xqkhn9qe", contractNameAtomNames, "atom.names registry", protocolNames, 30288946},
	{"cosmos12sseygvx4ykhp0df70ndg82l7p9a7ld7l0n0pptwj7c2h726cc9sahh4hz", contractNameAtomNames, "atom.names TLD manager", protocolNames, 30289440},
	{"cosmos1m962xzr0teztzlp39y7leefhqadxwxv4vg4jyzq6jxh64e93v9hsmg62rc", contractNameAtomNames, "atom.names marketplace", contractRoleMarketplace, 30289504},
	{"cosmos1tl0ptdmlz422z5pj20zujzyt3c96qp2g9vp83uj9pyj8q994tzhsl77adr", "Stargaze", "Stargaze marketplace", contractRoleMarketplace, 29099858},
	{"cosmos1qppyer6cu36kd4dchxqmcfa9u4rk2zgpc9vpmpd7eplhn8ghhyzs8najdv", "Starburst", "Starburst raffle", contractRoleRaffle, 30379639},
	{"cosmos1mjfkfegaywg6xsqr87uytsld9l7kaldj7sf7q593jdch8cascm6scnswx0", "Starburst", "Starburst multi-pool raffle", contractRoleRaffle, 30981928},
	{"cosmos1r9695xq7y0fd2nk06rv6llykay7ny27jrpq4de2w6eqja5p2mlvqfv8aa9", contractNameDAODAO, "DAO rewards distributor", attributeRewards, 30442266},
	{"cosmos19fprlcmzt2axrukx3xacsdpykv40nrdsv9l7jgnfrpwukgteu3zq3t2q9f", contractNameDAODAO, "DAO rewards distributor", attributeRewards, 30466450},
	{"cosmos15nafa2hazwmjs8yz844h5ry5yrzvz6q96a0300ge26vm8hcsrd4s7zxhyj", contractNameDAODAO, "DAO token-staked voting module", contractRoleDAOStaking, 30442266},
	{"cosmos1vuyxx6tl3j5wqkyhknvx6jzf8r53arhkpt565wqwsgdhz9dxx4sqyf4y9n", "Upload payment", "Upload payment", "payment", 31398417},
	{"cosmos1690hpscut3ltfkcm997fw2jws3tk0wkh7tnlntlavqkpg56hpdzsrjhy5r", contractNameInflow, "Inflow ATOM vault", contractRoleVault, 31588586},
	{"cosmos1d2hrvjvrvqvzdcffy2cy0p2gnytj48e2yg0lvnxk6knhmnt7cqvqetf09u", contractNameInflow, "Inflow vault", contractRoleVault, 31737292},
	{"cosmos12zktht39lk57zf8pqg287u6r8vu8zy6vkh7zfy0f5fn3zsddmx6sk9ru6y", contractNameInflow, "Inflow withdrawal router", contractRoleVault, 31737292},
	{"cosmos1jyx8zpvjxq2vdmquvrw6cwl2qp9slmnyy8dnpj3cgyvava2x7lmqhj3mmy", "Stargaze", "Stargaze reserve auction", contractRoleAuction, 31608661},
	{"cosmos1nhu3aqrlf3adlffvx3z5pyxntgulgj9dhfskwlp8fksx5mf7v9qqfjpgpd", "Governance Score", "Governance score rewards", attributeRewards, 31905560},
	{"cosmos1tj45y3729uu07pv8hq50pfx9u790rh6e0qssucs3yusxaag32t8svhlxgw", contractNameHydro, contractNameHydro, "hydro", 32538570},
	{"cosmos15pnu29h8gqpsfwhv2l8t2uv3n4fc6cv5k00tq0uk6c78h5ysql4sja3f3d", contractNameHydro, "Hydro tribute", protocolTribute, 32538598},
	{"cosmos17ryrjncgq8rrmxy0x8wrq0pcessp28xna4pw5sv506mfg3fhwj4qn89chn", contractNameHydro, "Hydro tribute", protocolTribute, 32538598},
	{"cosmos16qh8ly7zkae754a84tkv2hauee3z3u4mpcpsw0lchps03a72wrkske49ns", "Inscriptions", "Inscriptions auction and offers", "inscriptions", 29099858},
	{"cosmos17kzlqt4e0ncuwvluzfpuxed7trqw8wzk42kwnh3r6fwdnlzx8p6q0w69js", "ICS721", "ICS721 NFT bridge", "bridge", 29099858},
	{"cosmos1hcewh45u5e66wkuzm90t832qyxurng8l8r5kwjmzs2zn7cfau7gq96kdta", "ICS721", "ICS721 outgoing proxy", "bridge_helper", 29099858},
	{"cosmos1t0regae0m9tfylw6rjpwrcuytgn7u7pu2mnc0xtqgmauzlw6t4yqw64uys", "Mad Gumball", "Mad Gumball machine", "game", 29099858},
}

var knownContractsByAddress = func() map[string]KnownContract {
	m := make(map[string]KnownContract, len(KnownContracts))
	for _, c := range KnownContracts {
		m[c.Address] = c
	}
	return m
}()

// LookupContract returns the registry entry for an address, if any.
func LookupContract(addr string) (KnownContract, bool) {
	c, ok := knownContractsByAddress[addr]
	return c, ok
}

func contractKind(addr string) string {
	if c, ok := knownContractsByAddress[addr]; ok {
		return c.Kind
	}
	return ""
}
