package engine

import "crypto/subtle"

func (e *Engine) Authorized(token string) bool {
	if e.opts.APIKey == "" {
		return true
	}
	return token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(e.opts.APIKey)) == 1
}
