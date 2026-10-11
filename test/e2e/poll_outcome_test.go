//go:build e2e

package e2e

import "fmt"

// maxOutcomeLen caps a recorded description: a parse error from makeRequest
// embeds the whole response body, which can be an edge HTML error page.
const maxOutcomeLen = 300

// pollOutcome remembers what the latest attempt of a poll saw, so a timeout
// can say whether the route answered 404, answered from the wrong pod, or
// never answered at all, without logging every attempt.
type pollOutcome struct {
	attempts int
	last     string
}

func (o *pollOutcome) String() string {
	if o.attempts == 0 {
		return "no attempt completed"
	}

	return fmt.Sprintf("%d attempt(s), last: %s", o.attempts, o.last)
}

func (o *pollOutcome) record(desc string) {
	if len(desc) > maxOutcomeLen {
		desc = desc[:maxOutcomeLen] + " (truncated)"
	}

	o.attempts++
	o.last = desc
}

// annotate adds what the attempt saw after its request, such as a check
// against backend logs, without counting another attempt.
func (o *pollOutcome) annotate(note string) {
	o.last += "; " + note
}

// recordHTTP takes makeRequest's three results as returned.
func (o *pollOutcome) recordHTTP(echo *echoResponse, resp *echoHTTPResponse, err error) {
	switch {
	case err != nil && resp == nil:
		o.record("request failed: " + err.Error())
	case err != nil:
		o.record(fmt.Sprintf("status %d, then: %v", resp.StatusCode, err))
	case echo != nil && echo.Pod != "":
		o.record(fmt.Sprintf("status %d from pod %s", resp.StatusCode, echo.Pod))
	default:
		o.record(fmt.Sprintf("status %d", resp.StatusCode))
	}
}
