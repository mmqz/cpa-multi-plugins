// variant.go — per-variant platform constants for the merged trae plugin
// (v0.12.0). trae-cn (Trae Code CN) and trae-solo-cn (Trae SOLO CN) were
// separate plugins with identical logic except ClientID and the chat
// function; they are merged here, keyed by the account's variant.
package upstream

// clientIDByVariant / functionByVariant map auth variant → platform values
// (constants verified upstream; 禁止改动).
var (
	clientIDByVariant = map[string]string{
		"cn":   "ono9krqynydwx5", // non-solo (Trae Code CN)
		"solo": "en1oxy7wnw8j9n", // SOLO stable
	}
	functionByVariant = map[string]string{
		"cn":   "inline_chat",
		"solo": "solo_work_lite",
	}
)

// ClientIDFor returns the OAuth client id for a variant (default cn).
func ClientIDFor(variant string) string {
	if v, ok := clientIDByVariant[variant]; ok {
		return v
	}
	return clientIDByVariant["cn"]
}

// FunctionFor returns the llm_utils_chat function value for a variant.
func FunctionFor(variant string) string {
	if v, ok := functionByVariant[variant]; ok {
		return v
	}
	return functionByVariant["cn"]
}
