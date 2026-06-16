package server

import "go.uber.org/fx"

var Module = fx.Options(
	fx.Provide(NewGeminiFreeAPI),
	fx.Invoke(RegisterFiberLifecycle),
)
