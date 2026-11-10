package cards

import (
	"github.com/crueber/weave/internal/authz"
	"github.com/crueber/weave/internal/web"
)

// Claim row in the card sheet: a lazy fragment (template block "cards_claim" in
// _cards_claim.html) so the sheet template only carries one include line.

func renderClaim(c *web.Ctx, id string) error {
	card, err := cardGetCard(c.Env, id)
	if err != nil {
		return err
	}
	until := ""
	if card.ClaimedUntil != nil {
		if t, err := parseTime(*card.ClaimedUntil); err == nil {
			until = t.Local().Format("15:04")
		}
	}
	return c.RenderBlock("cards_modal.html", "cards_claim", map[string]any{
		"Card": card, "Until": until, "CanEdit": authz.BoardLevel(c.Env, card.BoardID) >= authz.Edit,
	})
}

func init() {
	web.Route("GET /ui/cards/{id}/claim", func(c *web.Ctx) error { return renderClaim(c, c.R.PathValue("id")) })
	web.Route("POST /ui/cards/{id}/release", func(c *web.Ctx) error {
		if _, err := c.Do("card.release"); err != nil {
			return err
		}
		return renderClaim(c, c.R.PathValue("id"))
	})
}
