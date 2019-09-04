package limiters

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/etcdserver/api/v3rpc/rpctypes"
	"github.com/coreos/etcd/mvcc/mvccpb"
	"github.com/go-redis/redis"
	"github.com/pkg/errors"
)

// TokenBucketState represents a state of a token bucket.
type TokenBucketState struct {
	// Last is the last time the state was updated (Unix timestamp in nanoseconds).
	Last int64
	// Available is the number of available tokens in the bucket.
	Available int64
}

// isZero returns true if the bucket state is zero valued.
func (s TokenBucketState) isZero() bool {
	return s.Last == 0 && s.Available == 0
}

// TokenBucketStateBackend interface encapsulates the logic of retrieving and persisting the state of a TokenBucket.
type TokenBucketStateBackend interface {
	// State gets the current state of the TokenBucket.
	State(ctx context.Context) (TokenBucketState, error)
	// SetState sets (persists) the current state of the TokenBucket.
	SetState(ctx context.Context, state TokenBucketState) error
}

// TokenBucket implements the https://en.wikipedia.org/wiki/Token_bucket algorithm.
type TokenBucket struct {
	Locker
	TokenBucketStateBackend
	Clock
	Logger
	// refillRate is the tokens refill rate.
	refillRate time.Duration
	// capacity is the bucket's capacity.
	capacity int64
	mu       sync.Mutex
}

// NewTokenBucket creates a new instance of TokenBucket.
func NewTokenBucket(capacity int64, refillRate time.Duration, locker Locker, tokenBucketStateBackend TokenBucketStateBackend, clock Clock, logger Logger) *TokenBucket {
	return &TokenBucket{
		Locker:                  locker,
		TokenBucketStateBackend: tokenBucketStateBackend,
		Clock:                   clock,
		Logger:                  logger,
		refillRate:              refillRate,
		capacity:                capacity,
	}
}

// Take takes tokens from the bucket.
//
// It returns a zero duration and a nil error if the bucket has sufficient amount of tokens.
//
// It returns ErrLimitExhausted if the amount of available tokens is less than requested. In this case the returned
// duration is the amount of time to wait to retry the request.
func (t *TokenBucket) Take(ctx context.Context, tokens int64) (time.Duration, error) {
	now := t.Clock.Now().UnixNano()
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.Lock(ctx); err != nil {
		return 0, err
	}
	defer func() {
		if err := t.Unlock(); err != nil {
			t.Logger.Log(err)
		}
	}()
	state, err := t.TokenBucketStateBackend.State(ctx)
	if err != nil {
		return 0, err
	}
	if state.isZero() {
		// Initially the bucket is full.
		state.Available = t.capacity
	}
	// Refill the bucket.
	tokensToAdd := (now - state.Last) / int64(t.refillRate)
	if tokensToAdd > 0 {
		state.Last = now
		if tokensToAdd+state.Available <= t.capacity {
			state.Available += tokensToAdd
		} else {
			state.Available = t.capacity
		}
	}

	if tokens > state.Available {
		return t.refillRate * time.Duration(tokens-state.Available), ErrLimitExhausted
	}
	// Take the tokens from the bucket.
	state.Available -= tokens
	if err := t.SetState(ctx, state); err != nil {
		return 0, err
	}
	return 0, nil
}

// Limit takes 1 token from the bucket.
func (t *TokenBucket) Limit(ctx context.Context) (time.Duration, error) {
	return t.Take(ctx, 1)
}

// TokenBucketInMemory is an in-memory implementation of TokenBucketStateBackend.
//
// The state is not shared nor persisted so it won't survive restarts or failures.
// Due to the local nature of the state the rate at which some endpoints are accessed can't be reliably predicted or
// limited.
//
// Although it can be used as a global rate limiter with a round-robin load-balancer.
type TokenBucketInMemory struct {
	state TokenBucketState
}

// NewTokenBucketInMemory creates a new instance of TokenBucketInMemory.
func NewTokenBucketInMemory() *TokenBucketInMemory {
	return &TokenBucketInMemory{}
}

// State returns the current bucket's state.
func (t *TokenBucketInMemory) State(ctx context.Context) (TokenBucketState, error) {
	return t.state, ctx.Err()
}

// SetState sets the current bucket's state.
func (t *TokenBucketInMemory) SetState(ctx context.Context, state TokenBucketState) error {
	t.state = state
	return ctx.Err()
}

const (
	etcdKeyTBLease     = "lease"
	etcdKeyTBAvailable = "available"
	etcdKeyTBLast      = "last"
)

// TokenBucketEtcd is an etcd implementation of a TokenBucketStateBackend.
//
// See https://github.com/etcd-io/etcd/blob/master/Documentation/learning/data_model.md
//
// etcd is designed to reliably store infrequently updated data, thus it should only be used for the API endpoints which
// are accessed less frequently than it can be processed by the rate limiter.
//
// Aggressive compaction and defragmentation has to be enabled in etcd to prevent the size of the storage
// to grow indefinitely: every change of the state of the bucket (every access) will create a new revision in etcd.
//
// It probably makes it impractical for the high load cases, but can be used to reliably and precisely rate limit an
// access to the business critical endpoints where each access must be reliably logged.
type TokenBucketEtcd struct {
	// prefix is the etcd key prefix.
	prefix      string
	cli         *clientv3.Client
	leaseID     clientv3.LeaseID
	ttl         time.Duration
	raceCheck   bool
	lastVersion int64
}

// NewTokenBucketEtcd creates a new TokenBucketEtcd instance.
// Prefix is used as an etcd key prefix for all keys stored in etcd by this algorithm.
// TTL is a TTL of the etcd lease in seconds used to store all the keys: all the keys are automatically deleted after
// the TTL expires.
//
// If raceCheck is true and the keys in etcd are modified in between State() and SetState() calls then
// ErrStateRaceCondition is returned.
// It does not add any significant overhead as it can be trivially checked on etcd side before updating the keys.
func NewTokenBucketEtcd(cli *clientv3.Client, prefix string, ttl time.Duration, raceCheck bool) *TokenBucketEtcd {
	return &TokenBucketEtcd{
		prefix:    prefix,
		cli:       cli,
		ttl:       ttl,
		raceCheck: raceCheck,
	}
}

// etcdKey returns a full etcd key from the provided key and prefix.
func etcdKey(prefix, key string) string {
	return fmt.Sprintf("%s/%s", prefix, key)
}

// parseEtcdInt64 parses the etcd value into int64.
func parseEtcdInt64(kv *mvccpb.KeyValue) (int64, error) {
	v, err := strconv.ParseInt(string(kv.Value), 10, 64)
	if err != nil {
		return 0, errors.Wrapf(err, "failed to parse key '%s' as int64", string(kv.Key))
	}
	return v, nil
}

func incPrefix(p string) string {
	b := []byte(p)
	b[len(b)-1]++
	return string(b)
}

// State gets the bucket's current state from etcd.
// If there is no state available in etcd then the initial bucket's state is returned.
func (t *TokenBucketEtcd) State(ctx context.Context) (TokenBucketState, error) {
	// Get all the keys under the prefix in a single request.
	r, err := t.cli.Get(ctx, t.prefix, clientv3.WithRange(incPrefix(t.prefix)))
	if err != nil {
		return TokenBucketState{}, errors.Wrapf(err, "failed to get keys in range ['%s', '%s') from etcd", t.prefix, incPrefix(t.prefix))
	}
	if len(r.Kvs) == 0 {
		// State not found, return zero valued state.
		return TokenBucketState{}, nil
	}
	state := TokenBucketState{}
	parsed := 0
	for _, kv := range r.Kvs {
		switch string(kv.Key) {
		case etcdKey(t.prefix, etcdKeyTBAvailable):
			v, err := parseEtcdInt64(kv)
			if err != nil {
				return TokenBucketState{}, err
			}
			state.Available = v
			parsed |= 1

		case etcdKey(t.prefix, etcdKeyTBLast):
			v, err := parseEtcdInt64(kv)
			if err != nil {
				return TokenBucketState{}, err
			}
			state.Last = v
			parsed |= 2
			t.lastVersion = kv.Version

		case etcdKey(t.prefix, etcdKeyTBLease):
			v, err := parseEtcdInt64(kv)
			if err != nil {
				return TokenBucketState{}, err
			}
			t.leaseID = clientv3.LeaseID(v)
			parsed |= 4
		}
	}
	if parsed != 7 {
		return TokenBucketState{}, errors.New("failed to get state from etcd: some keys are missing")
	}
	return state, nil
}

// createLease creates a new lease in etcd and updates the t.leaseID value.
func (t *TokenBucketEtcd) createLease(ctx context.Context) error {
	lease, err := t.cli.Grant(ctx, int64(t.ttl/time.Nanosecond))
	if err != nil {
		return errors.Wrap(err, "failed to create a new lease in etcd")
	}
	t.leaseID = lease.ID
	return nil
}

// save saves the state to etcd using the existing lease and the fencing token.
func (t *TokenBucketEtcd) save(ctx context.Context, state TokenBucketState) error {
	if !t.raceCheck {
		if _, err := t.cli.Txn(ctx).Then(
			clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBAvailable), fmt.Sprintf("%d", state.Available), clientv3.WithLease(t.leaseID)),
			clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBLast), fmt.Sprintf("%d", state.Last), clientv3.WithLease(t.leaseID)),
			clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBLease), fmt.Sprintf("%d", t.leaseID), clientv3.WithLease(t.leaseID)),
		).Commit(); err != nil {
			return errors.Wrap(err, "failed to commit a transaction to etcd")
		}
		return nil
	}
	// Put the keys only if they have not been modified since the most recent read.
	r, err := t.cli.Txn(ctx).If(
		clientv3.Compare(clientv3.Version(etcdKey(t.prefix, etcdKeyTBLast)), ">", t.lastVersion),
	).Else(
		clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBAvailable), fmt.Sprintf("%d", state.Available), clientv3.WithLease(t.leaseID)),
		clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBLast), fmt.Sprintf("%d", state.Last), clientv3.WithLease(t.leaseID)),
		clientv3.OpPut(etcdKey(t.prefix, etcdKeyTBLease), fmt.Sprintf("%d", t.leaseID), clientv3.WithLease(t.leaseID)),
	).Commit()
	if err != nil {
		return errors.Wrap(err, "failed to commit a transaction to etcd")
	}

	if !r.Succeeded {
		return nil
	}
	return ErrStateRaceCondition
}

// SetState updates the state of the bucket.
func (t *TokenBucketEtcd) SetState(ctx context.Context, state TokenBucketState) error {
	if t.leaseID == 0 {
		// Lease does not exist, create one.
		if err := t.createLease(ctx); err != nil {
			return err
		}
		// No need to send KeepAlive for the newly created lease: save the state immediately.
		return t.save(ctx, state)
	}
	// Send the KeepAlive request to extend the existing lease.
	if _, err := t.cli.KeepAliveOnce(ctx, t.leaseID); err == rpctypes.ErrLeaseNotFound {
		// Create a new lease since the current one has expired.
		if err := t.createLease(ctx); err != nil {
			return err
		}
	} else if err != nil {
		return errors.Wrapf(err, "failed to extend the lease '%d'", t.leaseID)
	}
	return t.save(ctx, state)
}

const (
	redisKeyTBAvailable = "available"
	redisKeyTBLast      = "last"
	redisKeyTBVersion   = "version"
)

func redisKey(prefix, key string) string {
	return fmt.Sprintf("%s/%s", prefix, key)
}

// TokenBucketRedis is a Redis implementation of a TokenBucketStateBackend.
//
// Redis is an in-memory key-value data storage which also supports persistence.
// It is a better choice for high load cases than etcd as it does not keep old values of the keys thus does not need
// the compaction/defragmentation.
//
// Although depending on a persistence and a cluster configuration some data might be lost in case of a failure
// resulting in an under-limiting the accesses to the service.
type TokenBucketRedis struct {
	cli         *redis.Client
	prefix      string
	ttl         time.Duration
	raceCheck   bool
	lastVersion int64
}

// NewTokenBucketRedis creates a new TokenBucketRedis instance.
// Prefix is the key prefix used to store all the keys used in this implementation in Redis.
// TTL is the TTL of the stored keys.
//
// If raceCheck is true and the keys in Redis are modified in between State() and SetState() calls then
// ErrStateRaceCondition is returned.
// This adds an extra overhead since a Lua script has to be executed on the Redis side which locks the entire database.
func NewTokenBucketRedis(cli *redis.Client, prefix string, ttl time.Duration, raceCheck bool) *TokenBucketRedis {
	return &TokenBucketRedis{cli: cli, prefix: prefix, ttl: ttl, raceCheck: raceCheck}
}

// State gets the bucket's state from Redis.
func (t *TokenBucketRedis) State(ctx context.Context) (TokenBucketState, error) {
	var values []interface{}
	var err error
	done := make(chan struct{}, 1)
	go func() {
		defer close(done)
		keys := []string{
			redisKey(t.prefix, redisKeyTBLast),
			redisKey(t.prefix, redisKeyTBAvailable),
		}
		if t.raceCheck {
			keys = append(keys, redisKey(t.prefix, redisKeyTBVersion))
		}
		values, err = t.cli.MGet(keys...).Result()
	}()

	select {
	case <-done:

	case <-ctx.Done():
		return TokenBucketState{}, ctx.Err()
	}

	if err != nil {
		return TokenBucketState{}, errors.Wrap(err, "failed to get keys from redis")
	}
	nilAny := false
	for _, v := range values {
		if v == nil {
			nilAny = true
			break
		}
	}
	if nilAny || err == redis.Nil {
		// Keys don't exist, return the initial state.
		return TokenBucketState{}, nil
	}

	last, err := strconv.ParseInt(values[0].(string), 10, 64)
	if err != nil {
		return TokenBucketState{}, err
	}
	available, err := strconv.ParseInt(values[1].(string), 10, 64)
	if err != nil {
		return TokenBucketState{}, err
	}
	if t.raceCheck {
		t.lastVersion, err = strconv.ParseInt(values[2].(string), 10, 64)
		if err != nil {
			return TokenBucketState{}, err
		}
	}
	return TokenBucketState{
		Last:      last,
		Available: available,
	}, nil
}

// SetState updates the state in Redis.
func (t *TokenBucketRedis) SetState(ctx context.Context, state TokenBucketState) error {
	var err error
	done := make(chan struct{}, 1)
	go func() {
		defer close(done)
		if !t.raceCheck {
			_, err = t.cli.TxPipelined(func(pipeliner redis.Pipeliner) error {
				if err := pipeliner.Set(redisKey(t.prefix, redisKeyTBLast), state.Last, t.ttl).Err(); err != nil {
					return err
				}
				return pipeliner.Set(redisKey(t.prefix, redisKeyTBAvailable), state.Available, t.ttl).Err()
			})
			return
		}
		var result interface{}
		result, err = t.cli.Eval(`
	local version = tonumber(redis.call('get', KEYS[1])) or 0
	if version > tonumber(ARGV[1]) then
		return 'RACE_CONDITION'
	end
	return {
		redis.call('incr', KEYS[1]),
		redis.call('pexpire', KEYS[1], ARGV[4]),
		redis.call('set', KEYS[2], ARGV[2], 'PX', ARGV[4]),
		redis.call('set', KEYS[3], ARGV[3], 'PX', ARGV[4]),
	}
	`, []string{
			redisKey(t.prefix, redisKeyTBVersion),
			redisKey(t.prefix, redisKeyTBLast),
			redisKey(t.prefix, redisKeyTBAvailable),
		},
			t.lastVersion,
			state.Last,
			state.Available,
			// TTL in milliseconds.
			int64(t.ttl/time.Microsecond)).Result()

		if err == nil {
			err = checkResponseFromRedis(result, []interface{}{t.lastVersion + 1, int64(1), "OK", "OK"})
		}
	}()

	select {
	case <-done:

	case <-ctx.Done():
		return ctx.Err()
	}

	return errors.Wrap(err, "failed to save keys to redis")
}
