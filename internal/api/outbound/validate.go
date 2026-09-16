package outbound

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const (
	maxHeaders      = 20
	maxHeadersBytes = 4096
	maxChannels     = 10
)

var channelRe = regexp.MustCompile(`^[a-zA-Z0-9\-_.]{1,128}$`)

// validateChannels bounds the count and enforces the Svix channel-name charset.
// A nil/empty slice is valid (an untagged, unfiltered message/endpoint).
func validateChannels(chs []string) error {
	if len(chs) > maxChannels {
		return fmt.Errorf("too many channels: %d (max %d)", len(chs), maxChannels)
	}
	for _, c := range chs {
		if !channelRe.MatchString(c) {
			return fmt.Errorf("invalid channel %q", c)
		}
	}
	return nil
}

// reservedHeaderNames are managed by dstream and may not be set as custom
// endpoint headers (case-insensitive; dstream- is a prefix match).
var reservedHeaderNames = map[string]struct{}{
	"content-type":      {},
	"webhook-id":        {},
	"webhook-timestamp": {},
	"webhook-signature": {},
}

func reservedHeader(name string) bool {
	l := strings.ToLower(name)
	if strings.HasPrefix(l, "dstream-") {
		return true
	}
	_, ok := reservedHeaderNames[l]
	return ok
}

func validateEndpointHeaders(h map[string]string) error {
	if len(h) > maxHeaders {
		return fmt.Errorf("too many headers: %d (max %d)", len(h), maxHeaders)
	}
	total := 0
	for k, v := range h {
		if !isTokenName(k) {
			return fmt.Errorf("header name %q is not a valid token", k)
		}
		if reservedHeader(k) {
			return fmt.Errorf("header %q is reserved", k)
		}
		total += len(k) + len(v)
	}
	if total > maxHeadersBytes {
		return fmt.Errorf("headers too large: %d bytes (max %d)", total, maxHeadersBytes)
	}
	return nil
}

// compileSchema reports whether raw is a compilable JSON Schema. Empty = ok.
func compileSchema(raw []byte) error {
	if len(raw) == 0 {
		return nil
	}
	_, err := buildSchema(raw)
	return err
}

// noExternalRefs refuses all external schema references ($ref to file://, http://, etc.).
// Event-type schemas must be self-contained: resolving external refs would let a stored
// schema read server files (LFI) and re-read them on every publish (DoS). Meta-schemas
// (the JSON Schema drafts) load from the library's embedded FS before this loader runs,
// so self-contained schemas still compile.
type noExternalRefs struct{}

func (noExternalRefs) Load(url string) (any, error) {
	return nil, fmt.Errorf("external schema references are not allowed: %s", url)
}

// buildSchema parses and compiles raw into a *jsonschema.Schema.
// ponytail: compiles per publish — add a compiled-schema cache keyed by a hash
// of the schema bytes only if publish-path profiling shows this hot.
func buildSchema(raw []byte) (*jsonschema.Schema, error) {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("schema is not valid json: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.UseLoader(noExternalRefs{})
	if err := c.AddResource("schema.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("schema.json")
}

// validatePayloadAgainstSchema validates payload against schema. Empty schema
// skips (returns nil).
func validatePayloadAgainstSchema(schema, payload []byte) error {
	if len(schema) == 0 {
		return nil
	}
	s, err := buildSchema(schema)
	if err != nil {
		return fmt.Errorf("stored schema invalid: %w", err)
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("payload is not valid json: %w", err)
	}
	if err := s.Validate(v); err != nil {
		return fmt.Errorf("payload does not match schema: %w", err)
	}
	return nil
}

// isTokenName is the strict RFC 7230 token check (no separators/controls).
func isTokenName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c <= ' ' || c >= 0x7f {
			return false
		}
		if strings.IndexByte("()<>@,;:\\\"/[]?={} \t", c) >= 0 {
			return false
		}
	}
	return true
}
