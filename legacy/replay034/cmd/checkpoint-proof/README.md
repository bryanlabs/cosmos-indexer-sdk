# Offline checkpoint next-header proof

The original replay reports state hashes through target minus one: the following
block header commits the target's application state. This standalone command
checks that last checkpoint without executing another block or changing the
original replay status, observation stream, export, or database.

Only pinned Hub 2 is supported. Source status/tail must identify the actual
loaded height and pinned release. Both cached blocks are decoded with the
pinned Tendermint codec and checked against their recorded block IDs and basic
internal block hashes. The next header must link to the current block, carry
the committed application root and recorded result root, and link the expected
validator-set hash. Missing evidence or any difference refuses the proof.

The source LOCK must already exist. The command opens it without O_CREATE,
takes a nonblocking shared lock (refusing the actual writer), and copies only
regular database files to a private retained directory. LevelDB opens only
that clone. No source file can be created by its opener, even for malformed
input. There is no automatic deletion of the clone. Proof output is a new
exclusive mode-0600 file outside the source checkpoint; stdout contains counts
only.

No Gaia constructor or cmn.Exit path loads untrusted commit metadata. The
native pinned multistore is mounted with exactly Gaia's persistent store set;
each loaded store's version/root must match its commit-info entry before the
multistore root is compared with the next header. Duplicated, extra, missing or
malformed store metadata fails with a fixed private diagnostic.

This is a checkpoint/root proof, not a new tax-import receipt, observation
completeness verdict, full filesystem audit or validator-signature-history
verification. It never changes the original verified-through field, and the
streaming importer must not silently reinterpret that field based on the
existence of this separate artifact. Original raw cached headers, source files
and exporter evidence remain required. Publication and production gates are
unchanged.
