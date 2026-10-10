package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
)

// Engine wires durable IM ingress to an existing Desktop conversation.
// User schedules, CLI sessions, model settings and skills are owned elsewhere.
type Engine struct {
	platformMu sync.RWMutex
	stopped    bool
	name       string
	agent      Agent
	platforms  []Platform
	cancel     context.CancelFunc
	stopOnce   sync.Once
}

func NewEngine(name string, agent Agent, platforms []Platform) *Engine {
	return &Engine{name: name, agent: agent, platforms: platforms}
}
func (e *Engine) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	e.cancel = cancel
	for _, p := range e.Platforms() {
		ingress, ok := p.(DurableIngressPlatform)
		if !ok {
			cancel()
			return fmt.Errorf("platform does not support durable ingress")
		}
		ingress.SetConversationHandler(e.agent.HandleConversation)
	}
	if err := e.agent.ResumeConversations(ctx, e.platforms); err != nil {
		cancel()
		return err
	}
	for _, p := range e.Platforms() {
		if err := p.Start(e.ReceiveMessage); err != nil {
			_ = e.Stop()
			return err
		}
	}
	return nil
}
func (e *Engine) ReceiveMessage(p Platform, m *Message) {
	if err := e.agent.HandleConversation(context.Background(), p, m); err != nil {
		slog.Error("conversation ingress failed", "error", err)
	}
}
func (e *Engine) Stop() (err error) {
	e.stopOnce.Do(func() {
		e.platformMu.Lock()
		e.stopped = true
		platforms := append([]Platform(nil), e.platforms...)
		e.platformMu.Unlock()
		if e.cancel != nil {
			e.cancel()
		}
		for _, p := range platforms {
			err = errors.Join(err, p.Stop())
		}
		err = errors.Join(err, e.agent.Stop())
	})
	return err
}

func (e *Engine) Platforms() []Platform {
	e.platformMu.RLock()
	defer e.platformMu.RUnlock()
	return append([]Platform(nil), e.platforms...)
}
func (e *Engine) ConnectPlatform(p Platform) error {
	e.platformMu.Lock()
	defer e.platformMu.Unlock()
	if e.stopped {
		return fmt.Errorf("Brodge is stopping")
	}
	if len(e.platforms) > 0 {
		return fmt.Errorf("Feishu already connected")
	}
	ingress, ok := p.(DurableIngressPlatform)
	if !ok {
		return fmt.Errorf("durable ingress required")
	}
	ingress.SetConversationHandler(e.agent.HandleConversation)
	if err := e.agent.ResumeConversations(context.Background(), []Platform{p}); err != nil {
		return err
	}
	if err := p.Start(e.ReceiveMessage); err != nil {
		return err
	}
	e.platforms = append(e.platforms, p)
	return nil
}

// ConfigurePlatform is used only before Start.
func (e *Engine) ConfigurePlatform(p Platform) { e.platforms = append(e.platforms, p) }
