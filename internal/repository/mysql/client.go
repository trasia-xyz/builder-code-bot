package mysql

import (
	"database/sql"
	"net"
	"strconv"
	"time"

	drivermysql "github.com/go-sql-driver/mysql"

	"hyperliquid-builder-code-bot/internal/config"
)

const (
	connectTimeout  = 5 * time.Second
	readTimeout     = 30 * time.Second
	writeTimeout    = 30 * time.Second
	maxOpenConns    = 10
	maxIdleConns    = 5
	connMaxLifetime = 30 * time.Minute
	connMaxIdleTime = 5 * time.Minute
)

// Open constructs a lazy MySQL connection pool. It intentionally does not
// ping so the service can start while MySQL is undergoing maintenance.
func Open(cfg config.MySQLConfig) (*sql.DB, error) {
	driverCfg := drivermysql.NewConfig()
	driverCfg.User = cfg.User
	driverCfg.Passwd = cfg.Password.Reveal()
	driverCfg.Net = "tcp"
	driverCfg.Addr = net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	driverCfg.DBName = cfg.Database
	driverCfg.Timeout = connectTimeout
	driverCfg.ReadTimeout = readTimeout
	driverCfg.WriteTimeout = writeTimeout
	driverCfg.ParseTime = true

	connector, err := drivermysql.NewConnector(driverCfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(maxOpenConns)
	db.SetMaxIdleConns(maxIdleConns)
	db.SetConnMaxLifetime(connMaxLifetime)
	db.SetConnMaxIdleTime(connMaxIdleTime)
	return db, nil
}
