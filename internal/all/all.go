// Package all links every domain package so their ops/routes self-register.
package all

import (
	_ "github.com/crueber/weave/internal/domain/admin"
	_ "github.com/crueber/weave/internal/domain/auth"
	_ "github.com/crueber/weave/internal/domain/boards"
	_ "github.com/crueber/weave/internal/domain/cardextras"
	_ "github.com/crueber/weave/internal/domain/cards"
	_ "github.com/crueber/weave/internal/domain/notify"
	_ "github.com/crueber/weave/internal/domain/projects"
)
