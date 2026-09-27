// Package dns provides utilities for detecting Kubernetes DNS configuration.
package dns

import (
	"bufio"
	"io"
	"os"
	"strings"
)

const (
	// DefaultClusterDomain is the default Kubernetes cluster domain.
	DefaultClusterDomain = "cluster.local"

	// ResolvConfPath is the default path to resolv.conf.
	ResolvConfPath = "/etc/resolv.conf"
)

// DetectClusterDomain attempts to detect the Kubernetes cluster domain
// from /etc/resolv.conf search domains.
//
// It looks for a search domain of the form "svc.<domain>" and extracts the
// cluster domain suffix.
//
// Returns the detected domain and true if successful,
// or empty string and false if detection failed.
func DetectClusterDomain() (string, bool) {
	return DetectClusterDomainFromFile(ResolvConfPath)
}

// DetectClusterDomainFromFile reads resolv.conf from a given path
// and extracts the cluster domain from search domains.
// Exported for testing purposes.
func DetectClusterDomainFromFile(path string) (string, bool) {
	file, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer file.Close()

	return parseResolvConf(file)
}

// parseResolvConf parses resolv.conf content and extracts the cluster domain
// from the search list, following the directive rules of Go's resolver: the
// last search or domain directive wins, a domain directive contributes only
// its first domain, and a domain directive with none is ignored. Unlike that
// resolver, a file that cannot be read to the end detects nothing.
func parseResolvConf(r io.Reader) (string, bool) {
	var domains []string

	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		switch {
		case len(fields) == 0:
		case fields[0] == "search":
			domains = fields[1:]
		case fields[0] == "domain" && len(fields) > 1:
			domains = fields[1:2]
		}
	}

	if scanner.Err() != nil {
		return "", false
	}

	domain := extractClusterDomain(domains)

	return domain, domain != ""
}

// extractClusterDomain finds cluster domain from search domains.
//
// Kubernetes DNS search domains typically look like:
//
//	default.svc.cluster.local svc.cluster.local cluster.local
//
// We look for a domain matching "svc.<cluster-domain>" pattern
// and extract the cluster domain part.
func extractClusterDomain(domains []string) string {
	for _, domain := range domains {
		// Look for "svc.<cluster-domain>" pattern
		clusterDomain, found := strings.CutPrefix(strings.TrimSuffix(domain, "."), "svc.")
		if found && clusterDomain != "" {
			return clusterDomain
		}
	}

	return ""
}
