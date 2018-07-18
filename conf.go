package qrpc

import (
	"context"
)

// ServerBinding contains binding infos
type ServerBinding struct {
	Addr                string
	Handler             Handler // handler to invoke
	DefaultReadTimeout  int
	DefaultWriteTimeout int
}

// SubFunc for subscribe callback
type SubFunc func(*Frame)

// ConnectionConfig is conf for Connection
type ConnectionConfig struct {
	Ctx          context.Context
	WriteTimeout int
	ReadTimeout  int
}
