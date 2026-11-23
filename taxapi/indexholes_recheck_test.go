package taxapi

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestIndexHoleBackupSafety(t *testing.T) {
	f, err := openIndexHoleBackup(RecheckIndexHolesOptions{})
	require.NoError(t, err)
	require.Nil(t, f)
	_, err = openIndexHoleBackup(RecheckIndexHolesOptions{Apply: true})
	require.ErrorContains(t, err, "requires --backup")

	path := filepath.Join(t.TempDir(), "backup.jsonl")
	f, err = openIndexHoleBackup(RecheckIndexHolesOptions{Apply: true, BackupPath: path})
	require.NoError(t, err)
	_, err = f.WriteString("original backup\n")
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	stat, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), stat.Mode().Perm())

	_, err = openIndexHoleBackup(RecheckIndexHolesOptions{Apply: true, BackupPath: path})
	require.ErrorIs(t, err, os.ErrExist)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "original backup\n", string(got))

	link := filepath.Join(t.TempDir(), "link.jsonl")
	require.NoError(t, os.Symlink(path, link))
	_, err = openIndexHoleBackup(RecheckIndexHolesOptions{Apply: true, BackupPath: link})
	require.ErrorIs(t, err, os.ErrExist)
}
