package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"
)

// resolvedRefsTrue is the "all refs resolved" condition the accepted-entry path
// receives when an entry's kinds and TLS refs are clean.
func resolvedRefsTrue() *metav1.Condition {
	return &metav1.Condition{
		Type:   string(gatewayv1.ListenerConditionResolvedRefs),
		Status: metav1.ConditionTrue,
		Reason: string(gatewayv1.ListenerReasonResolvedRefs),
	}
}

// TestAcceptedEntryConditions_UnsupportedProtocol pins that a ListenerSet entry
// whose protocol this controller cannot serve is Accepted=False /
// UnsupportedProtocol and not Programmed, mirroring the Gateway listener path.
func TestAcceptedEntryConditions_UnsupportedProtocol(t *testing.T) {
	t.Parallel()

	for _, proto := range []gatewayv1.ProtocolType{
		gatewayv1.TCPProtocolType, gatewayv1.TLSProtocolType, gatewayv1.UDPProtocolType,
	} {
		conds := acceptedEntryConditions(1, metav1.Now(), proto, resolvedRefsTrue())

		accepted := findCondition(conds, string(gatewayv1.ListenerConditionAccepted))
		require.NotNil(t, accepted)
		assert.Equal(t, metav1.ConditionFalse, accepted.Status, "%s entry must be Accepted=False", proto)
		assert.Equal(t, string(gatewayv1.ListenerReasonUnsupportedProtocol), accepted.Reason)

		programmed := findCondition(conds, string(gatewayv1.ListenerConditionProgrammed))
		require.NotNil(t, programmed)
		assert.Equal(t, metav1.ConditionFalse, programmed.Status,
			"an unservable-protocol entry must not be Programmed")
	}
}

// TestAcceptedEntryConditions_HTTPStaysAccepted confirms an HTTP entry with
// resolved refs stays Accepted=True and Programmed=True.
func TestAcceptedEntryConditions_HTTPStaysAccepted(t *testing.T) {
	t.Parallel()

	conds := acceptedEntryConditions(1, metav1.Now(), gatewayv1.HTTPProtocolType, resolvedRefsTrue())

	accepted := findCondition(conds, string(gatewayv1.ListenerConditionAccepted))
	require.NotNil(t, accepted)
	assert.Equal(t, metav1.ConditionTrue, accepted.Status)

	programmed := findCondition(conds, string(gatewayv1.ListenerConditionProgrammed))
	require.NotNil(t, programmed)
	assert.Equal(t, metav1.ConditionTrue, programmed.Status)
}
