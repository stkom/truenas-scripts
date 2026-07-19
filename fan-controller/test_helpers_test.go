package main

import "context"

type runnerFunc func(context.Context, string, ...string) (CommandResult, error)

func (fn runnerFunc) Run(ctx context.Context, path string, args ...string) (CommandResult, error) {
	return fn(ctx, path, args...)
}
