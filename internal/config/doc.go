// Package config provides configuration resolution from GatewayClassConfig CRD resources.
//
// The Resolver reads GatewayClass parametersRef, fetches the referenced GatewayClassConfig,
// and resolves all secrets to provide a complete ResolvedConfig for controllers.
package config
