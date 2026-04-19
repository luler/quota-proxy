package middleware

import (
	"sync"
	"sync/atomic"
	"time"

	"gin_base/app/config"

	"github.com/gin-gonic/gin"
)

type Runtime struct {
	Config          *config.Config
	QuotaMiddleware *QuotaMiddleware
	handler         gin.HandlerFunc
}

type RuntimeStore struct {
	current atomic.Value
	mu      sync.Mutex
}

func NewRuntimeStore(cfg *config.Config) (*RuntimeStore, error) {
	store := &RuntimeStore{}
	if err := store.Reload(cfg); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *RuntimeStore) Current() *Runtime {
	current, _ := s.current.Load().(*Runtime)
	return current
}

func (s *RuntimeStore) Reload(cfg *config.Config) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next, err := newRuntime(cfg)
	if err != nil {
		return err
	}

	prev, _ := s.current.Load().(*Runtime)
	s.current.Store(next)

	if prev != nil && prev.QuotaMiddleware != nil {
		go func(old *QuotaMiddleware) {
			time.Sleep(30 * time.Second)
			_ = old.Close()
		}(prev.QuotaMiddleware)
	}

	return nil
}

func (s *RuntimeStore) Handler() gin.HandlerFunc {
	return func(c *gin.Context) {
		current := s.Current()
		if current == nil || current.handler == nil {
			c.JSON(503, gin.H{"code": 503, "message": "运行时未初始化"})
			return
		}
		current.handler(c)
	}
}

func newRuntime(cfg *config.Config) (*Runtime, error) {
	qm, err := NewQuotaMiddleware(cfg)
	if err != nil {
		return nil, err
	}
	return &Runtime{Config: cfg, QuotaMiddleware: qm, handler: qm.Handler()}, nil
}
