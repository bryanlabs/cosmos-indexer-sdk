package pgtest

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestRetainedDestinationWithEmptyPassword(t *testing.T) {
	raw := os.Getenv("LEGACY_STORE_TEST_ADMIN_DSN")
	if raw == "" {
		t.Skip("private Postgres not supplied")
	}
	c, e := pgx.ParseConfig(raw)
	if e != nil {
		t.Fatal("private config invalid")
	}
	if c.Password != "" {
		t.Skip("empty-password regression requires a passwordless isolated fixture server")
	}
	// A genuinely empty password formerly swallowed the following dbname token.
	// This uses only the supplied isolated loopback server, never a default host.
	u := url.URL{Scheme: "postgres", Host: net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), User: url.UserPassword(c.User, ""), Path: "/" + c.Database, RawQuery: "sslmode=disable"}
	t.Setenv("LEGACY_STORE_TEST_ADMIN_DSN", u.String())
	fixture := OpenRetained(t)
	var owner, importer string
	if fixture.Owner.Raw("SELECT current_database()").Scan(&owner).Error != nil || fixture.Importer.Raw("SELECT current_database()").Scan(&importer).Error != nil {
		t.Fatal("fixture identity query failed")
	}
	if !strings.HasPrefix(owner, "legacy_replay_rehearsal_") || owner != importer || owner == c.Database {
		t.Fatal("fixture did not remain inside its new named database")
	}
}
