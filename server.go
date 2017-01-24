package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"crypto/tls"

	"github.com/flynn/flynn/pkg/httphelper"
	"github.com/flynn/flynn/pkg/postgres"
	"github.com/flynn/flynn/pkg/random"
	"github.com/flynn/flynn/pkg/resource"
	"github.com/flynn/flynn/pkg/shutdown"
	"github.com/jackc/pgx"
	"github.com/julienschmidt/httprouter"
	"golang.org/x/net/context"
)

const (
	disallowConns   = `UPDATE pg_database SET datallowconn = FALSE WHERE datname = $1`
	disconnectConns = `
SELECT pg_terminate_backend(pg_stat_activity.pid)
FROM pg_stat_activity
WHERE pg_stat_activity.datname = $1
  AND pid <> pg_backend_pid();`
)

var serviceUser = os.Getenv("PGUSER")
var serviceHost = os.Getenv("PGHOST")
var servicePass = os.Getenv("PGPASSWORD")
var servicePgSSL = os.Getenv("PGSSLMODE")
var systemPgsql = os.Getenv("FLYNN_POSTGRES")

func init() {
	if serviceUser == "" {
		serviceUser = "flynn"
	}
	if serviceHost == "" {
		panic("PGHOST must be set to the target database server hostname")
	}
	if servicePass == "" {
		panic("PGPASSWORD must be set to the database admin user password")
	}
	if servicePgSSL == "" {
		servicePgSSL = ""
	}
	if systemPgsql == "" {
		systemPgsql = "postgres"
	}
}

func main() {
	defer shutdown.Exit()

	// Don't use Wait wrapper, establish conn directly and wrap in DB
	pgxpool, err := pgx.NewConnPool(pgx.ConnPoolConfig{
		ConnConfig: pgx.ConnConfig{
			Host:     serviceHost,
			User:     serviceUser,
			Password: servicePass,
			Database: "postgres",
			TLSConfig: &tls.Config{ServerName: serviceHost, InsecureSkipVerify: true},
		},
	})
	if err != nil {
		shutdown.Fatal(err)
	}
	db := postgres.New(pgxpool, nil)
	api := &pgAPI{db}

	router := httprouter.New()
	router.POST("/databases", httphelper.WrapHandler(api.createDatabase))
	router.DELETE("/databases", httphelper.WrapHandler(api.dropDatabase))
	router.GET("/ping", httphelper.WrapHandler(api.ping))

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	addr := ":" + port

	handler := httphelper.ContextInjector("pg-external", httphelper.NewRequestLogger(router))
	shutdown.Fatal(http.ListenAndServe(addr, handler))
}

type pgAPI struct {
	db *postgres.DB
}

func (p *pgAPI) createDatabase(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	username, password, database := random.Hex(16), random.Hex(16), random.Hex(16)

	if err := p.db.Exec(fmt.Sprintf(`CREATE USER "%s" WITH PASSWORD '%s'`, username, password)); err != nil {
		httphelper.Error(w, err)
		return
	}
	if err := p.db.Exec(fmt.Sprintf(`GRANT "%s" TO "%s"`, username, serviceUser)); err != nil {
		p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, username))
		httphelper.Error(w, err)
		return
	}
	if err := p.db.Exec(fmt.Sprintf(`CREATE DATABASE "%s" WITH OWNER = "%s"`, database, username)); err != nil {
		p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, username))
		httphelper.Error(w, err)
		return
	}

	url := fmt.Sprintf("postgres://%s:%s@%s:5432/%s", username, password, serviceHost, database)
	httphelper.JSON(w, 200, resource.Resource{
		ID: fmt.Sprintf("/databases/%s:%s", username, database),
		Env: map[string]string{
			"FLYNN_POSTGRES": systemPgsql,
			"PGHOST":         serviceHost,
			"PGUSER":         username,
			"PGPASSWORD":     password,
			"PGDATABASE":     database,
			"DATABASE_URL":   url,
		},
	})
}

func (p *pgAPI) dropDatabase(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	id := strings.SplitN(strings.TrimPrefix(req.FormValue("id"), "/databases/"), ":", 2)
	if len(id) != 2 || id[1] == "" {
		httphelper.ValidationError(w, "id", "is invalid")
		return
	}

	// disable new connections to the target database
	if err := p.db.Exec(disallowConns, id[1]); err != nil {
		httphelper.Error(w, err)
		return
	}

	// terminate current connections
	if err := p.db.Exec(disconnectConns, id[1]); err != nil {
		httphelper.Error(w, err)
		return
	}

	if err := p.db.Exec(fmt.Sprintf(`DROP DATABASE "%s"`, id[1])); err != nil {
		httphelper.Error(w, err)
		return
	}

	if err := p.db.Exec(fmt.Sprintf(`DROP USER "%s"`, id[0])); err != nil {
		httphelper.Error(w, err)
		return
	}

	w.WriteHeader(200)
}

func (p *pgAPI) ping(ctx context.Context, w http.ResponseWriter, req *http.Request) {
	if err := p.db.Exec("SELECT 1"); err != nil {
		httphelper.Error(w, err)
		return
	}
	w.WriteHeader(200)
}
