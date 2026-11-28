package personal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/kelos-dev/aloop"
	"golang.org/x/oauth2"
)

type Google struct {
	client *http.Client
	drive  bool
	email  string
}

type googleStatus int

func (s googleStatus) Error() string { return fmt.Sprintf("Google API returned HTTP %d", s) }

// Token endpoint responses can include credentials in their body or description.
func googleRequestError(err error) error {
	var tokenErr *oauth2.RetrieveError
	if errors.As(err, &tokenErr) {
		switch tokenErr.ErrorCode {
		case "invalid_grant", "invalid_client", "invalid_request", "unauthorized_client", "invalid_scope", "access_denied":
			return fmt.Errorf("Google token endpoint returned %s (HTTP %d)", tokenErr.ErrorCode, tokenErr.Response.StatusCode)
		default:
			return fmt.Errorf("Google token endpoint returned HTTP %d", tokenErr.Response.StatusCode)
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	return errors.New("Google request failed; check network connectivity and the response format")
}

func (g *Google) get(ctx context.Context, endpoint string, query url.Values) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := g.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var tokenErr *oauth2.RetrieveError
		if errors.As(err, &tokenErr) && tokenErr.ErrorCode == "invalid_grant" {
			return nil, aloop.ToolError("Google authorization expired or was revoked; reconnect the account")
		}
		return nil, googleRequestError(err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, googleStatus(resp.StatusCode)
	}
	return resp.Body, nil
}

func (g *Google) gmailJSON(ctx context.Context, path string, query url.Values, out any) error {
	body, err := g.get(ctx, "https://gmail.googleapis.com/gmail/v1/users/me/"+path, query)
	if err != nil {
		return err
	}
	defer body.Close()
	return json.NewDecoder(body).Decode(out)
}

var googleFileID = regexp.MustCompile(`^[a-zA-Z0-9_-]+$`)

func (g *Google) readDoc(ctx context.Context, c aloop.Call) (string, error) {
	id := c.Args["id"]
	if !googleFileID.MatchString(id) {
		return "", aloop.ToolError("supply the file ID from a Google Docs link")
	}
	body, err := g.get(ctx, "https://www.googleapis.com/drive/v3/files/"+id+"/export", url.Values{"mimeType": {"text/plain"}})
	if errors.Is(err, googleStatus(http.StatusForbidden)) || errors.Is(err, googleStatus(http.StatusNotFound)) || errors.Is(err, googleStatus(http.StatusBadRequest)) {
		return "", aloop.ToolError("this document cannot be exported; check the file ID, Drive access, and document type")
	}
	if err != nil {
		return "", err
	}
	defer body.Close()
	text, err := io.ReadAll(io.LimitReader(body, 16385))
	if len(text) > 16384 {
		return strings.ToValidUTF8(string(text[:16384]), "") + "\n[Document truncated]", err
	}
	return string(text), err
}
