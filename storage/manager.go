package storage

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bitleak/lmstfy/config"
	"github.com/bitleak/lmstfy/engine"
	"github.com/bitleak/lmstfy/helper"
	"github.com/bitleak/lmstfy/storage/lock"
	"github.com/bitleak/lmstfy/storage/persistence/model"
	"github.com/bitleak/lmstfy/storage/persistence/spanner"
	"github.com/bitleak/lmstfy/storage/pumper"
	"github.com/go-redis/redis/v8"
	"github.com/sirupsen/logrus"
)

const (
	maxJobBatchSize = 128

	defaultLockExpiry   = 15 * time.Second
	defaultPumpInterval = 3 * time.Second
)

type Manager struct {
	pools   map[string]engine.Engine
	pumpers map[string]pumper.Pumper

	mu       sync.Mutex
	redisCli *redis.Client
	storage  Persistence
}

var manager *Manager

func Init(cfg *config.Config) (err error) {
	manager, err = NewManger(cfg)
	return err
}

func Get() *Manager {
	return manager
}

func NewManger(cfg *config.Config) (*Manager, error) {
	if cfg.SecondaryStorage == nil {
		return nil, errors.New("nil second storage config")
	}
	storage, err := spanner.NewSpanner(cfg.SecondaryStorage)
	if err != nil {
		return nil, err
	}
	redisCli := helper.NewRedisClient(&cfg.AdminRedis, nil)
	if redisCli.Ping(context.Background()).Err() != nil {
		return nil, fmt.Errorf("create redis client err: %w", err)
	}
	return &Manager{
		redisCli: redisCli,
		storage:  storage,
		pools:    make(map[string]engine.Engine),
		pumpers:  make(map[string]pumper.Pumper),
	}, nil
}

func (m *Manager) PumpFn(name string, pool engine.Engine, threshold int64) func() bool {
	return func() bool {
		now := time.Now()
		req := &model.JobDataReq{
			PoolName: name,
			// FIXME: don't hard code deadline here
			ReadyTime: now.Unix() + threshold,
			Count:     maxJobBatchSize,
		}
		ctx := context.TODO()
		jobs, err := m.storage.GetReadyJobs(ctx, req)
		if err != nil {
			logrus.Errorf("Get ready jobs err: %v", err)
			return false
		}
		jobsID := make([]string, 0)
		for _, job := range jobs {
			j := engine.NewJob(job.Namespace, job.Queue, job.Body, uint32(job.ExpiredTime),
				uint32(job.ReadyTime-now.Unix()), uint16(job.Tries), job.JobID)
			_, err := pool.Publish(j)
			if err != nil {
				logrus.Errorf("Publish:%v failed with error %v", job.JobID, err)
				return false
			}
			jobsID = append(jobsID, job.JobID)
		}

		if _, err := m.storage.DelJobs(ctx, jobsID); err != nil {
			logrus.Errorf("LoopPump delete jobs failed:%v", err)
			return false
		}
		return len(jobsID) == maxJobBatchSize
	}
}

func (m *Manager) AddPool(name string, pool engine.Engine, threshold int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	redisLock := lock.NewRedisLock(m.redisCli, name, defaultLockExpiry)
	pumper := pumper.NewDefault(redisLock, defaultPumpInterval)
	go pumper.Loop(m.PumpFn(name, pool, threshold))
}

func (m *Manager) AddJob(ctx context.Context, job *model.JobData) error {
	return m.storage.BatchAddJobs(ctx, []*model.JobData{job})
}

func (m *Manager) GetJobByID(ctx context.Context, ID string) ([]*model.JobData, error) {
	return m.storage.BatchGetJobsByID(ctx, []string{ID})
}

func (m *Manager) Shutdown() {
	// Stop and release pumper here
}