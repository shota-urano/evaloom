package api

import (
	"net/url"
	"os"
)

// Symlinks and case aliases must not bypass Registry path uniqueness.
func sameRepository(name string, others []string) (bool, error) {
	info, err := os.Stat(name)
	if err != nil {
		return false, err
	}
	for _, other := range others {
		if other == name {
			return true, nil
		}
		if otherInfo, err := os.Stat(other); err == nil && os.SameFile(info, otherInfo) {
			return true, nil
		}
	}
	return false, nil
}

// Credentials are not configuration history. Keep only the repository identity.
func auditRepositoryURL(value string) string {
	if remote, err := url.Parse(value); err == nil && remote.Scheme != "" && remote.Host != "" {
		remote.User, remote.RawQuery, remote.Fragment = nil, "", ""
		return remote.String()
	}
	return value
}
