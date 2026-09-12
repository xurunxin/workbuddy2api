package server

import (
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// Validate references without fetching remote media or reading local files.
// Keep error messages independent of the URL, which may contain credentials.
func validateMediaURL(value, kind string) error {
	if strings.HasPrefix(value, "data:") {
		header, data, ok := strings.Cut(value, ",")
		if !ok || !strings.HasPrefix(header, "data:"+kind+"/") || !strings.HasSuffix(header, ";base64") || validateMediaBase64(data) != nil {
			return fmt.Errorf("%s Data URI must contain base64 encoded %s data", kind, kind)
		}
		return nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s URL must use HTTP(S) or a base64 Data URI", kind)
	}
	return nil
}

func validateMediaBase64(data string) error {
	n, err := io.Copy(io.Discard, base64.NewDecoder(base64.StdEncoding, strings.NewReader(data)))
	if err != nil || n == 0 {
		return fmt.Errorf("media must contain non-empty base64 data")
	}
	return nil
}
