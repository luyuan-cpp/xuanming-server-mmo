package svc

import (
	"fmt"

	"db/internal/config"
)

type ServiceContext struct {
	Config config.Config
}

func NewServiceContext() *ServiceContext {
	if err := config.AppConfig.ValidateGlobalAuthority(); err != nil {
		panic(fmt.Errorf("global DB configuration rejected; refusing to start: %w", err))
	}
	return &ServiceContext{Config: config.AppConfig}
}
