package ports

// RegionalAlertRepository binds all alert operations to one immutable assignment.
// Binding scopes storage; it does not grant execution or caller authorization.
// Unbound repositories expose local history by ID/token/list and only the active
// local generation for open-alert lookup and creation.
type RegionalAlertRepository interface {
	ForAssignment(probeID string, generation int64) (AlertRepository, error)
}
