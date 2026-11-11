package launch

import "strings"

// IdentityEnvKeys are the variables that make up an AMQ participant
// identity.
var IdentityEnvKeys = []string{"AM_ROOT", "AM_BASE_ROOT", "AM_SESSION", "AM_ME", "AM_ROOT_ID", "AM_BASE_ROOT_ID"}

// Identity is an AMQ participant identity: the values of IdentityEnvKeys.
// A store that records an identity for a harness session embeds it, so the
// fields have one JSON form.
type Identity struct {
	Root       string `json:"root"`
	BaseRoot   string `json:"base_root"`
	Session    string `json:"session,omitempty"`
	Me         string `json:"me"`
	RootID     string `json:"root_id"`
	BaseRootID string `json:"base_root_id"`
}

// IdentityFromEnv reads the identity variables from env, a list of
// KEY=value entries; the first entry of a key wins.
func IdentityFromEnv(env []string) Identity {
	get := func(key string) string {
		prefix := key + "="
		for _, entry := range env {
			if v, ok := strings.CutPrefix(entry, prefix); ok {
				return v
			}
		}
		return ""
	}
	return Identity{
		Root: get("AM_ROOT"), BaseRoot: get("AM_BASE_ROOT"), Session: get("AM_SESSION"),
		Me: get("AM_ME"), RootID: get("AM_ROOT_ID"), BaseRootID: get("AM_BASE_ROOT_ID"),
	}
}

// Env returns the identity as the value of each of IdentityEnvKeys.
func (id Identity) Env() map[string]string {
	return map[string]string{
		"AM_ROOT": id.Root, "AM_BASE_ROOT": id.BaseRoot, "AM_SESSION": id.Session,
		"AM_ME": id.Me, "AM_ROOT_ID": id.RootID, "AM_BASE_ROOT_ID": id.BaseRootID,
	}
}
