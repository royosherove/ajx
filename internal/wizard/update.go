package wizard

import (
	"errors"

	"charm.land/huh/v2"
)

// UpdateChoice asks once for the scope of an available update.
func UpdateChoice(summary string, x IO) (string, error) {
	choice := "all"
	field := huh.NewSelect[string]().Title("Update AJX").Description(summary).Options(
		huh.NewOption("CLI + skill (recommended)", "all"),
		huh.NewOption("CLI only — keep my current skill", "cli"),
		huh.NewOption("Cancel", "cancel"),
	).Value(&choice)
	err := x.apply(huh.NewForm(huh.NewGroup(field))).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "cancel", nil
	}
	return choice, err
}
