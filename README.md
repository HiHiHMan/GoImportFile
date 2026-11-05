# GoImportFile

Go web application for importing CSV, XLSX and legacy XLS files into an existing SQL Server table.

## Design

- CSV: Go standard library `encoding/csv`
- XLSX: Excelize row iterator
- XLS: `github.com/extrame/xls`
- SQL Server: pure-Go `github.com/denisenkom/go-mssqldb` driver; no OLEDB/ODBC dependency
- SQL writes: SQL Server bulk-copy protocol through `go-mssqldb`'s `CreateBulkContext`
- One import at a time; rows are processed in batches (default 500)
- Header names are mapped case-insensitively to existing SQL table columns
- Credentials are read from environment variables, never from source code

## Configure

1. Install Go 1.23 or newer.
2. Copy `.env.example` to `.env` and set environment variables in your service environment. The application does not load .env automatically.
3. Set a valid SQL Server host/IP, port, SQL login and password.
4. Confirm the target table exists (default `An_DB.dbo.Tbl_ImportPool`).
5. Run:

```powershell
go mod tidy
$env:SQLSERVER_HOST="192.168.x.x"
$env:SQLSERVER_USER="your_login"
$env:SQLSERVER_PASSWORD="your_password"
go run .
```

The submitted server value `192.168.234.456` is not a valid IPv4 address. Replace it with the correct address before running.

## Run

Open `http://localhost:8080`. Select a CSV/XLSX/XLS file and upload.

API:
- `GET /health`
- `POST /api/import` multipart field name: `file`

The API response reports imported row count and elapsed time. The target table is configured by environment variables; clients cannot choose arbitrary SQL identifiers.

## Import behavior

- Append only. Existing rows are not deleted.
- The first row is treated as column headers.
- XLSX and XLS read the first worksheet only.
- Empty cells are sent as SQL NULL.
- Columns not present in the file are omitted so SQL defaults/nullable definitions apply.
- Header names must match existing SQL column names (case-insensitive).
- Validation and persistent import logs are disabled as requested. Essential file-type/header/table checks remain enabled for safe operation.
- Bulk-copy commits are made per batch. If a later batch fails, previously committed batches remain inserted; this is append mode, not an all-file atomic transaction.

## Resource controls

- One import at a time.
- Default batch size: 500 rows.
- XLSX is read using a row iterator; CSV uses a streaming reader.
- The legacy XLS reader parses the binary workbook internally, so its memory use can be higher than CSV/XLSX. For large datasets, prefer XLSX or CSV.
- Upload size is bounded by `IMPORT_MAX_UPLOAD_MB`.
- SQL connection pool is intentionally small.

## Notes

The legacy XLS parser is included separately from Excelize because XLS is the older binary Excel format and is not the same format as XLSX.
