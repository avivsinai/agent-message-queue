package cli

import (
	"os"

	"github.com/avivsinai/agent-message-queue/internal/route"
)

// TreeRelation is a physical-filesystem relationship; see route.TreeRelation.
type TreeRelation = route.TreeRelation

const (
	TreeRelationUnknown   = route.TreeRelationUnknown
	TreeRelationSame      = route.TreeRelationSame
	TreeRelationDifferent = route.TreeRelationDifferent
)

func resolveTreeIdentityToken(path string) (string, error) {
	return routeWalker().TreeIdentityToken(path)
}

func relateTrees(a, b string) TreeRelation { return routeWalker().RelateTrees(a, b) }

func verifyTreeIdentityToken(path, token string) TreeRelation {
	return routeWalker().VerifyTreeIdentityToken(path, token)
}

func verifyTreeIdentityInfo(info os.FileInfo, token string) TreeRelation {
	return route.VerifyTreeIdentityInfo(info, token)
}

func sameTreeIdentity(a, b string) bool { return routeWalker().SameTreeIdentity(a, b) }

func validTreeIdentityToken(token string) bool { return route.ValidTreeIdentityToken(token) }
