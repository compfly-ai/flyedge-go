package identity

import "strings"

// TenantFromDID extracts the org-short tenant from a DID of the form
// did:compfly:<org_short>:<fingerprint>. Returns "" if the DID isn't in that shape.
func TenantFromDID(did string) string {
	parts := strings.Split(did, ":")
	if len(parts) < 4 || parts[0] != "did" {
		return ""
	}
	return parts[2]
}
