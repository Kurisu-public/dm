// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package loader

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/errors"
	"github.com/pingcap/failpoint"
	tmysql "github.com/pingcap/parser/mysql"
	"go.uber.org/zap"

	"github.com/pingcap/dm/dm/config"
	"github.com/pingcap/dm/pkg/conn"
	tcontext "github.com/pingcap/dm/pkg/context"
	"github.com/pingcap/dm/pkg/log"
	"github.com/pingcap/dm/pkg/retry"
	"github.com/pingcap/dm/pkg/terror"
	"github.com/pingcap/dm/pkg/utils"
)

// DBConn represents a live DB connection
// it's not thread-safe
type DBConn struct {
	cfg      *config.SubTaskConfig
	baseConn *conn.BaseConn
}

func (conn *DBConn) querySQL(ctx *tcontext.Context, query string, args ...interface{}) (*sql.Rows, error) {
	if conn == nil || conn.baseConn == nil {
		return nil, terror.ErrDBUnExpect.Generate("database connection not valid")
	}

	params := retry.Params{
		RetryCount:         10,
		FirstRetryDuration: time.Second,
		BackoffStrategy:    retry.Stable,
		IsRetryableFn: func(retryTime int, err error) bool {
			if retry.IsRetryableError(err) {
				ctx.L().Warn("query statement", zap.Int("retry", retryTime),
					zap.String("query", utils.TruncateString(query, -1)),
					zap.String("argument", utils.TruncateInterface(args, -1)),
					log.ShortError(err))
				return true
			}
			return false
		},
	}

	ret, _, err := conn.baseConn.ApplyRetryStrategy(
		ctx,
		params,
		func(ctx *tcontext.Context) (interface{}, error) {
			startTime := time.Now()
			ret, err := conn.baseConn.QuerySQL(ctx, query, args...)
			if err == nil {
				cost := time.Since(startTime)
				queryHistogram.WithLabelValues(conn.cfg.Name).Observe(cost.Seconds())
				if cost.Seconds() > 1 {
					ctx.L().Warn("query statement",
						zap.String("query", utils.TruncateString(query, -1)),
						zap.String("argument", utils.TruncateInterface(args, -1)),
						zap.Duration("cost time", cost))
				}
			}
			return ret, err
		})
	if err != nil {
		ctx.L().Error("query statement failed after retry",
			zap.String("query", utils.TruncateString(query, -1)),
			zap.String("argument", utils.TruncateInterface(args, -1)),
			log.ShortError(err))
		return nil, err
	}
	return ret.(*sql.Rows), nil
}

func (conn *DBConn) executeSQL(ctx *tcontext.Context, queries []string, args ...[]interface{}) error {
	if len(queries) == 0 {
		return nil
	}

	if conn == nil || conn.baseConn == nil {
		return terror.ErrDBUnExpect.Generate("database connection not valid")
	}

	params := retry.Params{
		RetryCount:         10,
		FirstRetryDuration: 2 * time.Second,
		BackoffStrategy:    retry.LinearIncrease,
		IsRetryableFn: func(retryTime int, err error) bool {
			ctx.L().Warn("execute statements", zap.Int("retry", retryTime),
				zap.String("queries", utils.TruncateInterface(queries, -1)),
				zap.String("arguments", utils.TruncateInterface(args, -1)),
				log.ShortError(err))
			tidbExecutionErrorCounter.WithLabelValues(conn.cfg.Name).Inc()
			return retry.IsRetryableError(err)
		},
	}
	_, _, err := conn.baseConn.ApplyRetryStrategy(
		ctx,
		params,
		func(ctx *tcontext.Context) (interface{}, error) {
			startTime := time.Now()
			_, err := conn.baseConn.ExecuteSQL(ctx, queries, args...)
			failpoint.Inject("LoadExecCreateTableFailed", func(val failpoint.Value) {
				errCode, err1 := strconv.ParseUint(val.(string), 10, 16)
				if err1 != nil {
					ctx.L().Fatal("failpoint LoadExecCreateTableFailed's value is invalid", zap.String("val", val.(string)))
				}

				if len(queries) == 1 && strings.Contains(queries[0], "CREATE TABLE") {
					err = &mysql.MySQLError{uint16(errCode), ""}
					ctx.L().Warn("executeSQL failed", zap.String("failpoint", "LoadExecCreateTableFailed"), zap.Error(err))
				}
			})
			if err == nil {
				cost := time.Since(startTime)
				txnHistogram.WithLabelValues(conn.cfg.Name).Observe(cost.Seconds())
				if cost.Seconds() > 1 {
					ctx.L().Warn("transaction execute successfully", zap.Duration("cost time", cost))
				}
			}
			return nil, err
		})

	if err != nil {
		ctx.L().Error("execute statements failed after retry",
			zap.String("queries", utils.TruncateInterface(queries, -1)),
			zap.String("arguments", utils.TruncateInterface(args, -1)),
			log.ShortError(err))
	}

	return err
}

// Close release db connection resource, return it to BaseDB.db connection pool
func (conn *DBConn) Close() error {
	if conn == nil || conn.baseConn == nil {
		return nil
	}
	return conn.baseConn.Close()
}

func createConn(ctx context.Context, cfg *config.SubTaskConfig) (*conn.BaseDB, *DBConn, error) {
	baseDB, err := conn.DefaultDBProvider.Apply(cfg.To)
	if err != nil {
		return nil, nil, terror.WithScope(terror.DBErrorAdapt(err, terror.ErrDBDriverError), terror.ScopeDownstream)
	}
	baseConn, err := baseDB.GetBaseConn(ctx)
	if err != nil {
		return nil, nil, terror.WithScope(terror.DBErrorAdapt(err, terror.ErrDBDriverError), terror.ScopeDownstream)
	}
	return baseDB, &DBConn{baseConn: baseConn, cfg: cfg}, nil
}

func createConns(tctx *tcontext.Context, cfg *config.SubTaskConfig, workerCount int) (*conn.BaseDB, []*DBConn, error) {
	baseDB, err := conn.DefaultDBProvider.Apply(cfg.To)
	if err != nil {
		return nil, nil, terror.WithScope(terror.DBErrorAdapt(err, terror.ErrDBDriverError), terror.ScopeDownstream)
	}
	conns := make([]*DBConn, 0, workerCount)
	for i := 0; i < workerCount; i++ {
		baseConn, err := baseDB.GetBaseConn(tctx.Context())
		if err != nil {
			terr := baseDB.Close()
			if terr != nil {
				tctx.L().Error("failed to close baseDB", zap.Error(terr))
			}
			return nil, nil, terror.WithScope(terror.DBErrorAdapt(err, terror.ErrDBDriverError), terror.ScopeDownstream)
		}
		conns = append(conns, &DBConn{baseConn: baseConn, cfg: cfg})
	}
	return baseDB, conns, nil
}

func isErrDBExists(err error) bool {
	return isMySQLError(err, tmysql.ErrDBCreateExists)
}

func isErrTableExists(err error) bool {
	return isMySQLError(err, tmysql.ErrTableExists)
}

func isErrDupEntry(err error) bool {
	return isMySQLError(err, tmysql.ErrDupEntry)
}

func isMySQLError(err error, code uint16) bool {
	err = errors.Cause(err)
	e, ok := err.(*mysql.MySQLError)
	return ok && e.Number == code
}
