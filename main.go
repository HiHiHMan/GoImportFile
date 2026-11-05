package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	_ "embed"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
)

type Config struct {
	SQLHost, SQLUser, SQLPassword, SQLDatabase, SQLPort, SQLEncrypt string
	Schema, Table, HTTPAddr string
	BatchSize, MaxUploadMB int
}

type App struct {
	cfg Config
	db *sql.DB
	gate chan struct{}
}

type importResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Rows    int64  `json:"rows,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// loadDotEnv loads KEY=VALUE pairs from .env without overriding existing
// environment variables. The .env file is optional and must not be committed.
func loadDotEnv(path string) error {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\uFEFF"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if key == "" {
			continue
		}

		// Remove matching surrounding quotes.
		if len(value) >= 2 {
			if (value[0] == '"' && value[len(value)-1] == '"') ||
				(value[0] == '\'' && value[len(value)-1] == '\'') {
				value = value[1 : len(value)-1]
			}
		}

		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return err
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return nil
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" { return value }
	return fallback
}
func envInt(key string, fallback int) int {
	n, err := strconv.Atoi(env(key, strconv.Itoa(fallback)))
	if err != nil || n < 1 { return fallback }
	return n
}

func loadConfig() Config {
	return Config{
		SQLHost: env("SQLSERVER_HOST", "127.0.0.1"),
		SQLPort: env("SQLSERVER_PORT", "1433"),
		SQLUser: env("SQLSERVER_USER", ""),
		SQLPassword: os.Getenv("SQLSERVER_PASSWORD"),
		SQLDatabase: env("SQLSERVER_DATABASE", "SystemValue"),
		SQLEncrypt: env("SQLSERVER_ENCRYPT", "disable"),
		Schema: env("IMPORT_SCHEMA", "dbo"),
		Table: env("IMPORT_TABLE", "Tbl_ImportPool"),
		HTTPAddr: env("HTTP_ADDR", ":8080"),
		BatchSize: envInt("IMPORT_BATCH_SIZE", 500),
		MaxUploadMB: envInt("IMPORT_MAX_UPLOAD_MB", 1024),
	}
}

func main() {
	if err := loadDotEnv(".env"); err != nil {
		log.Fatalf("Failed to load .env: %v", err)
	}

	cfg := loadConfig()
	if cfg.SQLUser == "" || cfg.SQLPassword == "" {
		log.Fatal("SQLSERVER_USER and SQLSERVER_PASSWORD must be configured in .env or environment variables")
	}
	query := url.Values{}
	query.Set("database", cfg.SQLDatabase)
	query.Set("encrypt", cfg.SQLEncrypt)
	query.Set("connection timeout", "10")
	query.Set("app name", "GoImportFile")
	dsn := (&url.URL{Scheme: "sqlserver", User: url.UserPassword(cfg.SQLUser, cfg.SQLPassword), Host: net.JoinHostPort(cfg.SQLHost, cfg.SQLPort), RawQuery: query.Encode()}).String()
	db, err := sql.Open("sqlserver", dsn)
	if err != nil { log.Fatal(err) }
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil { log.Fatalf("SQL Server connection failed: %v", err) }

	app := &App{cfg: cfg, db: db, gate: make(chan struct{}, 1)}
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.index)
	mux.HandleFunc("/health", app.health)
	mux.HandleFunc("/api/import", app.importFile)

	server := &http.Server{Addr: cfg.HTTPAddr, Handler: securityHeaders(mux), ReadHeaderTimeout: 10*time.Second}
	log.Printf("GoImportFile listening on %s; target=%s.%s; batch=%d", cfg.HTTPAddr, cfg.Schema, cfg.Table, cfg.BatchSize)
	log.Fatal(server.ListenAndServe())
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

//go:embed index.html
var indexHTML []byte

func (a *App) index(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" { http.NotFound(w,r); return }
	if r.Method != http.MethodGet { http.Error(w,"method not allowed",http.StatusMethodNotAllowed); return }
	w.Header().Set("Content-Type","text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
}
func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx,cancel:=context.WithTimeout(r.Context(),3*time.Second); defer cancel()
	if err:=a.db.PingContext(ctx); err!=nil { writeJSON(w,http.StatusServiceUnavailable,importResponse{Message:"SQL Server unavailable"}); return }
	writeJSON(w,http.StatusOK,importResponse{Success:true,Message:"ok"})
}
func writeJSON(w http.ResponseWriter,status int,v importResponse) {
	w.Header().Set("Content-Type","application/json; charset=utf-8"); w.WriteHeader(status); _=json.NewEncoder(w).Encode(v)
}
func (a *App) importFile(w http.ResponseWriter,r *http.Request) {
	if r.Method!=http.MethodPost { writeJSON(w,http.StatusMethodNotAllowed,importResponse{Message:"method not allowed"}); return }
	select { case a.gate<-struct{}{}: defer func(){<-a.gate}(); default: writeJSON(w,http.StatusConflict,importResponse{Message:"มีงาน Import กำลังทำงานอยู่ กรุณาลองใหม่"}); return }
	r.Body=http.MaxBytesReader(w,r.Body,int64(a.cfg.MaxUploadMB)*1024*1024)
	if err:=r.ParseMultipartForm(32<<20); err!=nil { writeJSON(w,http.StatusBadRequest,importResponse{Message:"ไฟล์ไม่ถูกต้องหรือมีขนาดเกินกำหนด"}); return }
	file,header,err:=r.FormFile("file"); if err!=nil { writeJSON(w,http.StatusBadRequest,importResponse{Message:"กรุณาเลือกไฟล์"}); return }; defer file.Close()
	ext:=strings.ToLower(filepath.Ext(header.Filename))
	if ext!=".csv" && ext!=".xlsx" && ext!=".xls" { writeJSON(w,http.StatusBadRequest,importResponse{Message:"รองรับเฉพาะ CSV, XLSX และ XLS"}); return }
	tmp,err:=os.CreateTemp("","goimport-*"+ext); if err!=nil { writeJSON(w,http.StatusInternalServerError,importResponse{Message:"ไม่สามารถเตรียมไฟล์ได้"}); return }
	tmpName:=tmp.Name(); defer os.Remove(tmpName)
	if _,err=io.Copy(tmp,file); err!=nil { tmp.Close(); writeJSON(w,http.StatusBadRequest,importResponse{Message:"อ่านไฟล์ไม่สำเร็จ"}); return }
	if err=tmp.Close(); err!=nil { writeJSON(w,http.StatusInternalServerError,importResponse{Message:"บันทึกไฟล์ชั่วคราวไม่สำเร็จ"}); return }

	start:=time.Now()
	ctx,cancel:=context.WithTimeout(r.Context(),2*time.Hour); defer cancel()
	rows,err:=a.runImport(ctx,tmpName,ext)
	if err!=nil {
		status:=http.StatusBadRequest
		if errors.Is(err,context.DeadlineExceeded) { status=http.StatusGatewayTimeout } else if strings.Contains(err.Error(),"SQL") { status=http.StatusInternalServerError }
		writeJSON(w,status,importResponse{Message:err.Error(),Rows:rows,Elapsed:time.Since(start).Round(time.Millisecond).String()}); return
	}
	writeJSON(w,http.StatusOK,importResponse{Success:true,Message:"นำเข้าข้อมูลสำเร็จ",Rows:rows,Elapsed:time.Since(start).Round(time.Millisecond).String()})
}
