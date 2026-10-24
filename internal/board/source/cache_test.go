package source

import (
	"os"
	"testing"
	"time"
)

// Pro finding 11: an entry for another account or repo set is a miss, even
// when its bytes sit at the other key's path.
func TestCacheEntryServesOnlyItsOwnKey(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := DirCache{Dir: t.TempDir()}
	key := CacheKey{Host: "github.com", Repos: []string{"octo-org/demo"}, Account: "octo-owner", Schema: GitHubQuerySchema}
	if err := c.Put(key, now, []byte(`{"data":{}}`)); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := c.Get(key, now, time.Minute); !ok {
		t.Fatal("own key missed")
	}
	otherAccount := key
	otherAccount.Account = "octo-other"
	otherRepos := key
	otherRepos.Repos = []string{"octo-org/demo", "octo-org/tools"}
	data, err := os.ReadFile(c.path(key))
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []CacheKey{otherAccount, otherRepos} {
		if err := os.WriteFile(c.path(other), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := c.Get(other, now, time.Minute); ok {
			t.Errorf("entry for %+v served %+v", key, other)
		}
	}
}
