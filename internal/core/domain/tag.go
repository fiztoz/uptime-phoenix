package domain

// Tag represents a key-value tag that can be assigned to monitors.
type Tag struct {
	ID    int64
	Name  string
	Color string // hex color, e.g. "#666666"
}

// MonitorTag represents the assignment of a tag to a monitor with an optional value.
type MonitorTag struct {
	ID        int64
	MonitorID int64
	TagID     int64
	Value     string
}
