package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"
)

type CallLocator interface {
	Register(ctx context.Context, callID, value string, ttl time.Duration) error
	Renew(ctx context.Context, callID string, ttl time.Duration) error
	RenewMany(ctx context.Context, callIDs []string, ttl time.Duration) error
	Delete(ctx context.Context, callID string) error
	Close() error
}

type disabledLocator struct {
	reason string
}

func (l disabledLocator) Register(context.Context, string, string, time.Duration) error {
	return fmt.Errorf("call locator is disabled: %s", l.reason)
}
func (l disabledLocator) Renew(context.Context, string, time.Duration) error       { return nil }
func (l disabledLocator) RenewMany(context.Context, []string, time.Duration) error { return nil }
func (l disabledLocator) Delete(context.Context, string) error                     { return nil }
func (l disabledLocator) Close() error                                             { return nil }

type redisCallLocator struct {
	client *redis.Client
	log    *slog.Logger
}

func NewCallLocator(ctx context.Context, cfg *Config, log *slog.Logger) (CallLocator, error) {
	if cfg.RedisAddr == "" {
		return disabledLocator{reason: "redis_addr is required"}, nil
	}
	client := redis.NewClient(&redis.Options{
		Addr: cfg.RedisAddr,
		DB:   cfg.RedisDB,
	})
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &redisCallLocator{client: client, log: log.With("component", "call_locator")}, nil
}

func (l *redisCallLocator) Register(ctx context.Context, callID, value string, ttl time.Duration) error {
	if err := l.client.Set(ctx, locatorKey(callID), value, ttl).Err(); err != nil {
		return fmt.Errorf("set call locator: %w", err)
	}
	l.log.Info("registered call locator", "sipCallID", callID, "value", value, "ttl", ttl.String())
	return nil
}

func (l *redisCallLocator) Renew(ctx context.Context, callID string, ttl time.Duration) error {
	if err := l.client.Expire(ctx, locatorKey(callID), ttl).Err(); err != nil {
		return fmt.Errorf("renew call locator: %w", err)
	}
	return nil
}

func (l *redisCallLocator) RenewMany(ctx context.Context, callIDs []string, ttl time.Duration) error {
	if len(callIDs) == 0 {
		return nil
	}
	_, err := l.client.Pipelined(ctx, func(pipe redis.Pipeliner) error {
		for _, callID := range callIDs {
			pipe.Expire(ctx, locatorKey(callID), ttl)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("renew call locators: %w", err)
	}
	return nil
}

func (l *redisCallLocator) Delete(ctx context.Context, callID string) error {
	if err := l.client.Del(ctx, locatorKey(callID)).Err(); err != nil {
		return fmt.Errorf("delete call locator: %w", err)
	}
	l.log.Info("deleted call locator", "sipCallID", callID)
	return nil
}

func (l *redisCallLocator) Close() error {
	return l.client.Close()
}

func locatorKey(callID string) string {
	return "loc:" + callID
}
