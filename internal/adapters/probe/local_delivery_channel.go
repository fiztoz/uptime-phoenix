package probe

import (
	"reflect"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
	"github.com/fiztoz/uptime-phoenix/internal/core/ports"
)

var _ ports.LocalProbeConfigChannelComparer = LocalConfigEncoder{}

// SameLocalDeliveryChannel compares provider settings and the referenced template,
// not the probe-wide document revision or unrelated monitor/dependency settings.
// Neither document supplies current send authority.
func (e LocalConfigEncoder) SameLocalDeliveryChannel(previous []byte, current domain.LocalProbeConfigDefinition, notificationID int64) (bool, error) {
	old, err := DecodeLocalConfigSnapshot(previous)
	if err != nil {
		return false, domain.ErrValidation
	}
	document, err := e.EncodeLocal(current)
	if err != nil {
		return false, domain.ErrValidation
	}
	defer clear(document)
	next, err := DecodeLocalConfigSnapshot(document)
	if err != nil || old.HubID != next.HubID || old.ProbeID != next.ProbeID || old.Revision > next.Revision {
		return false, domain.ErrValidation
	}
	find := func(s ConfigSnapshot) (*ConfigChannel, *ConfigTemplate) {
		for _, channel := range s.NotificationChannels {
			if channel.ID != notificationID || !channel.Active {
				continue
			}
			channel.Version = 0
			if channel.TemplateID == nil {
				return &channel, nil
			}
			for _, template := range s.NotificationTemplates {
				if template.ID == *channel.TemplateID {
					template.Version = 0
					return &channel, &template
				}
			}
		}
		return nil, nil
	}
	before, beforeTemplate := find(old)
	after, afterTemplate := find(next)
	return before != nil && after != nil && reflect.DeepEqual(before, after) && reflect.DeepEqual(beforeTemplate, afterTemplate), nil
}
