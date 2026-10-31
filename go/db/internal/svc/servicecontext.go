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
		panic(fmt.Errorf("全球DB配置拒绝启动: %w", err))
	}
	return &ServiceContext{Config: config.AppConfig}
}
