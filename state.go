package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"time"
)

type State struct {
	Initialized bool              `json:"initialized"`
	Assignments map[string]*Entry `json:"assignments"`
	Alerts      map[string]bool   `json:"alerts"` // one-shot warnings currently raised
	// BadCredHash is the fingerprint of credentials GSU last rejected. While it
	// matches the current .env, login is skipped so repeated bad attempts can't
	// lock the account. Cleared when the credentials change.
	BadCredHash string `json:"bad_cred_hash,omitempty"`
}

type Entry struct {
	Announced bool      `json:"announced"`
	Sent      []string  `json:"sent"`
	Due       time.Time `json:"due"`
}

func (e *Entry) HasSent(label string) bool {
	for _, s := range e.Sent {
		if s == label {
			return true
		}
	}
	return false
}

func (e *Entry) MarkSent(label string) {
	if !e.HasSent(label) {
		e.Sent = append(e.Sent, label)
	}
}

func NewState() *State {
	return &State{Assignments: map[string]*Entry{}, Alerts: map[string]bool{}}
}

func LoadState(path string) (*State, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	s := NewState()
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Assignments == nil {
		s.Assignments = map[string]*Entry{}
	}
	if s.Alerts == nil {
		s.Alerts = map[string]bool{}
	}
	return s, nil
}

// Save writes atomically via a temp file and rename.
func (s *State) Save(path string) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
