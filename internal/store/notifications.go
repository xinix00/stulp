package store

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const maxNotificationExcerpt = 500

type Notification struct {
	ID        string `json:"id"`
	AppID     string `json:"appId"`
	Excerpt   string `json:"excerpt"`
	CreatedAt string `json:"createdAt"`
}

func (s *Store) CreateNotification(_ context.Context, appID, excerpt string) (Notification, error) {
	excerpt = strings.TrimSpace(excerpt)
	if excerpt == "" {
		return Notification{}, errors.New("notification excerpt is required")
	}
	if len([]rune(excerpt)) > maxNotificationExcerpt {
		return Notification{}, fmt.Errorf("notification excerpt exceeds %d characters", maxNotificationExcerpt)
	}
	notification := Notification{
		ID: newID(), AppID: appID, Excerpt: excerpt,
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	s.mu.Lock()
	s.doc.Notifications = append(s.doc.Notifications, notification)
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return Notification{}, fmt.Errorf("create notification: %w", err)
	}
	s.publish(Event{Manager: "notifications", Type: "notification.create", ID: notification.ID, Data: notification})
	return notification, nil
}

func (s *Store) Notifications(_ context.Context, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	notifications := append([]Notification(nil), s.doc.Notifications...)
	sort.SliceStable(notifications, func(left, right int) bool {
		return notifications[left].CreatedAt > notifications[right].CreatedAt
	})
	if len(notifications) > limit {
		notifications = notifications[:limit]
	}
	return notifications, nil
}

func (s *Store) DeleteNotification(_ context.Context, id string) error {
	s.mu.Lock()
	before := len(s.doc.Notifications)
	s.doc.Notifications = removeWhere(s.doc.Notifications, func(item Notification) bool { return item.ID == id })
	if len(s.doc.Notifications) == before {
		s.mu.Unlock()
		return fmt.Errorf("notification %q does not exist", id)
	}
	err := s.saveLocked()
	s.mu.Unlock()
	if err != nil {
		return fmt.Errorf("delete notification %q: %w", id, err)
	}
	s.publish(Event{Manager: "notifications", Type: "notification.delete", ID: id})
	return nil
}
