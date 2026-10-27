package main

import (
	"os"
	"path/filepath"

	"session-memory/internal/errcode"
	"session-memory/internal/transport"
)

// envWeiyunToken names the environment variable holding the user's Weiyun
// access token. It is deliberately never stored in the config file; for
// background runs (autosync) it is kept in the OS credential store.
const envWeiyunToken = "WEIYUN_MCP_TOKEN"

// weiyunTokenKey is the credential-store id of the saved token.
const weiyunTokenKey = "weiyun-mcp-token"

// weiyunToken is the token from the environment, else the saved one.
func (a *App) weiyunToken() string {
	if t := os.Getenv(envWeiyunToken); t != "" {
		return t
	}
	if b, err := a.Keys.Load(weiyunTokenKey); err == nil {
		return string(b)
	}
	return ""
}

// saveWeiyunToken keeps the environment's token in the credential store so
// that background runs, which do not see the shell environment, can use it.
func (a *App) saveWeiyunToken() error {
	t := os.Getenv(envWeiyunToken)
	if t == "" {
		if a.weiyunToken() != "" {
			return nil
		}
		return errcode.New(errcode.Usage, "set "+envWeiyunToken+" first")
	}
	return a.Keys.Store(weiyunTokenKey, []byte(t))
}

// weiyunCache is the local working copy of the Weiyun vault folder.
func (a *App) weiyunCache() string { return filepath.Join(a.Cfg.StateDir(), "weiyun-cache") }

// weiyunMirror returns the mirror for the configured Weiyun directory, or nil
// when Weiyun is not the transport.
func (a *App) weiyunMirror(name string) (*transport.Mirror, error) {
	if name == "" {
		return nil, nil
	}
	token := a.weiyunToken()
	if token == "" {
		return nil, errcode.New(errcode.Usage, "set "+envWeiyunToken+" to use the Weiyun transport")
	}
	if err := os.MkdirAll(a.weiyunCache(), 0700); err != nil {
		return nil, err
	}
	c := transport.NewWeiyunClient(token)
	if u := os.Getenv("WEIYUN_MCP_URL"); u != "" {
		c.URL = u
	}
	return transport.NewMirror(transport.NewFolder(a.weiyunCache()), c, name), nil
}
