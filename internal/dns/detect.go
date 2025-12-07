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
// It looks for a search domain matching the pattern "*.svc.<domain>"
// and extracts the cluster domain suffix.
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

// parseResolvConf parses resolv.conf content and extracts the cluster domain.
func parseResolvConf(r io.Reader) (string, bool) {
	scanner := bufio.NewScanner(r)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// Skip comments and empty lines
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		// Look for search directive
		if strings.HasPrefix(line, "search ") {
			domains := strings.Fields(line)[1:] // Skip "search" keyword
			if domain := extractClusterDomain(domains); domain != "" {
				return domain, true
			}
		}
	}

	// Check for scanner errors (e.g., read errors)
	if scanner.Err() != nil {
		return "", false
	}

	return "", false
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
		if clusterDomain, found := strings.CutPrefix(domain, "svc."); found && clusterDomain != "" {
			return clusterDomain
		}
	}

	return ""
}
