package sqlite

import (
	"maps"

	"github.com/QuantumNous/astrlink/core/contract"
)

// cloneForkHTTPConnection copies the request-rule fields the fork adds to an
// HTTP connection. Upstream's cache clone copies the connection by value, which
// would leave these maps and slices shared with the cached record.
func cloneForkHTTPConnection(connection *contract.HTTPConnection) {
	if connection == nil {
		return
	}
	connection.ExtraHeaders = maps.Clone(connection.ExtraHeaders)
	if connection.ModelRules != nil {
		rules := make([]contract.ModelRule, len(connection.ModelRules))
		for index, rule := range connection.ModelRules {
			rules[index] = rule.Clone()
		}
		connection.ModelRules = rules
	}
}
