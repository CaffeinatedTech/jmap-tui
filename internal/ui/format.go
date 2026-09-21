package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/CaffeinatedTech/jmap-tui/internal/mail"
)

// RelativeDate renders receivedAt as a compact relative stamp under 7 days
// old ("45m", "3h", "2d"), "Jan 02" within the current year, and
// "Sep ’25" for older mail — everything fits an 8-column date field (FR-D1).
func RelativeDate(at time.Time, now time.Time) string {
	if at.IsZero() {
		return ""
	}
	d := now.Sub(at)
	switch {
	case d < 0:
		// Future stamps (clock skew or scheduled mail): show absolute.
	case d < time.Minute:
		return "now"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
	if at.Year() == now.Year() {
		return at.Format("Jan 02")
	}
	return fmt.Sprintf("%s ’%02d", at.Format("Jan"), at.Year()%100)
}

// HumanSize renders a byte count compactly ("1.2K", "3.4M").
func HumanSize(n int64) string {
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fK", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1fM", float64(n)/(1024*1024))
	}
}

// DisplayName renders an address list as a short column value: the first
// address's name, else its email local part (FR-C3: Sent shows recipients,
// so callers pass the address list they want displayed).
func DisplayName(addrs []mail.Address) string {
	if len(addrs) == 0 {
		return ""
	}
	a := addrs[0]
	if a.Name != "" {
		return a.Name
	}
	if i := strings.Index(a.Email, "@"); i > 0 {
		return a.Email[:i]
	}
	return a.Email
}
