package tunnel

// ProxyHTTPSentinelsForTest lists every error ProxyHTTP can return, so the
// external tests can pin that nothing else reaches cloudflared. The list is
// maintained by hand: a new sentinel returned from ProxyHTTP goes here too.
func ProxyHTTPSentinelsForTest() []error {
	return []error{errNilTracedRequest, errHandlerPanic}
}
