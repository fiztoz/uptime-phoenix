package repository

import (
	"time"

	"github.com/uptrace/bun"

	"github.com/fiztoz/uptime-phoenix/internal/core/domain"
)

// MonitorModel maps the monitors table.
type MonitorModel struct {
	bun.BaseModel `bun:"table:monitors"`

	ID                  int64           `bun:"id,pk,autoincrement"`
	UserID              int64           `bun:"user_id"`
	Name                string          `bun:"name,notnull"`
	Description         string          `bun:"description"`
	Owner               string          `bun:"owner,notnull,default:''"`
	InheritGroupOwner   bool            `bun:"inherit_group_owner,notnull,default:false"`
	Type                string          `bun:"type,notnull"`
	Active              bool            `bun:"active,notnull,default:true"`
	Interval            int             `bun:"check_interval,notnull,default:60"`
	RetryInterval       int             `bun:"retry_interval,notnull,default:0"`
	MaxRetries          int             `bun:"max_retries,notnull,default:0"`
	Timeout             float64         `bun:"timeout,notnull,default:30.0"`
	GroupID             *int64          `bun:"group_id"`
	Weight              int             `bun:"weight,notnull,default:2000"`
	PushName            string          `bun:"push_token"` // push monitor identifier
	ProxyID             *int64          `bun:"proxy_id"`
	SkipCertVerify      bool            `bun:"tls_ignore,notnull,default:false"`
	CertExpiryNotify    bool            `bun:"cert_expiry_notify,notnull,default:false"`
	AcceptedStatusCodes StringListField `bun:"accepted_statuscodes"`
	ResendInterval      int             `bun:"resend_interval,notnull,default:0"`
	UpsideDown          bool            `bun:"upside_down,notnull,default:false"`
	Config              JSONField       `bun:"config,notnull"`
	DockerHostID        *int64          `bun:"docker_host_id"`
	WorkerID            *string         `bun:"worker_id"`
	LeasedAt            *time.Time      `bun:"leased_at"`
	CreatedAt           time.Time       `bun:"created_at,notnull"`
	UpdatedAt           time.Time       `bun:"updated_at,notnull"`
}

// ToDomain converts a MonitorModel to a domain.Monitor.
func (m *MonitorModel) ToDomain() *domain.Monitor {
	return &domain.Monitor{
		ID:                  m.ID,
		UserID:              m.UserID,
		Name:                m.Name,
		Description:         m.Description,
		Owner:               m.Owner,
		InheritGroupOwner:   m.InheritGroupOwner,
		Type:                m.Type,
		Active:              m.Active,
		Interval:            m.Interval,
		RetryInterval:       m.RetryInterval,
		MaxRetries:          m.MaxRetries,
		Timeout:             m.Timeout,
		Config:              m.Config.ToMap(),
		AcceptedStatusCodes: []string(m.AcceptedStatusCodes),
		ProxyID:             m.ProxyID,
		UpsideDown:          m.UpsideDown,
		ResendInterval:      m.ResendInterval,
		PushToken:           m.PushName,
		GroupID:             m.GroupID,
		Weight:              m.Weight,
		TLSIgnore:           m.SkipCertVerify,
		CertExpiryNotify:    m.CertExpiryNotify,
		DockerHostID:        m.DockerHostID,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
	}
}

// MonitorModelFromDomain converts a domain.Monitor to a MonitorModel.
func MonitorModelFromDomain(m *domain.Monitor) *MonitorModel {
	return &MonitorModel{
		ID:                  m.ID,
		UserID:              m.UserID,
		Name:                m.Name,
		Description:         m.Description,
		Owner:               m.Owner,
		InheritGroupOwner:   m.InheritGroupOwner,
		Type:                m.Type,
		Active:              m.Active,
		Interval:            m.Interval,
		RetryInterval:       m.RetryInterval,
		MaxRetries:          m.MaxRetries,
		Timeout:             m.Timeout,
		GroupID:             m.GroupID,
		Weight:              m.Weight,
		PushName:            m.PushToken,
		ProxyID:             m.ProxyID,
		SkipCertVerify:      m.TLSIgnore,
		CertExpiryNotify:    m.CertExpiryNotify,
		AcceptedStatusCodes: StringListField(m.AcceptedStatusCodes),
		ResendInterval:      m.ResendInterval,
		UpsideDown:          m.UpsideDown,
		Config:              JSONField(m.Config),
		DockerHostID:        m.DockerHostID,
		CreatedAt:           m.CreatedAt,
		UpdatedAt:           m.UpdatedAt,
	}
}
