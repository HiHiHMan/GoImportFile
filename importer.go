package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/extrame/xls"
	"github.com/xuri/excelize/v2"
)

type rowReader interface { Next() ([]string,error); Close() error }

type csvRows struct { f *os.File; r *csv.Reader }
func openCSV(path string)(rowReader,error) {
	f,err:=os.Open(path); if err!=nil{return nil,err}
	r:=csv.NewReader(f); r.FieldsPerRecord=-1; r.ReuseRecord=true
	return &csvRows{f:f,r:r},nil
}
func(c *csvRows)Next()([]string,error){ rec,err:=c.r.Read(); if err!=nil{return nil,err}; out:=append([]string(nil),rec...); return out,nil }
func(c *csvRows)Close()error{return c.f.Close()}

type xlsxRows struct { f *excelize.File; rows *excelize.Rows }
func openXLSX(path string)(rowReader,error) {
	f,err:=excelize.OpenFile(path); if err!=nil{return nil,err}
	sheets:=f.GetSheetList(); if len(sheets)==0 { f.Close(); return nil,fmt.Errorf("Excel ไม่มี Sheet") }
	rows,err:=f.Rows(sheets[0]); if err!=nil {f.Close();return nil,err}
	return &xlsxRows{f:f,rows:rows},nil
}
func(x *xlsxRows)Next()([]string,error){ if !x.rows.Next(){ if err:=x.rows.Error();err!=nil{return nil,err};return nil,io.EOF }; return x.rows.Columns() }
func(x *xlsxRows)Close()error{ _=x.rows.Close(); return x.f.Close() }

type xlsRows struct { book *xls.WorkBook; sheet *xls.WorkSheet; index int }
func openXLS(path string)(rowReader,error) {
	book,err:=xls.Open(path,"utf-8"); if err!=nil{return nil,err}
	if book.NumSheets()==0{return nil,fmt.Errorf("Excel ไม่มี Sheet")}
	sheet:=book.GetSheet(0); if sheet==nil{return nil,fmt.Errorf("อ่าน Sheet แรกไม่ได้")}
	return &xlsRows{book:book,sheet:sheet},nil
}
func(x *xlsRows)Next()([]string,error) {
	for x.index<=int(x.sheet.MaxRow) {
		row:=x.sheet.Row(x.index); x.index++
		if row==nil { continue }
		cols:=make([]string,row.LastCol()); for i:=range cols { cols[i]=row.Col(i) }
		return cols,nil
	}
	return nil,io.EOF
}
func(x *xlsRows)Close()error{return nil}

func(a *App)runImport(ctx context.Context,path,ext string)(int64,error) {
	var rr rowReader; var err error
	switch ext {case ".csv":rr,err=openCSV(path);case ".xlsx":rr,err=openXLSX(path);case ".xls":rr,err=openXLS(path)}
	if err!=nil{return 0,fmt.Errorf("เปิดไฟล์ไม่สำเร็จ: %w",err)}
	defer rr.Close()
	header,err:=rr.Next(); if err!=nil{return 0,fmt.Errorf("อ่าน Header ไม่สำเร็จ: %w",err)}
	if len(header)==0{return 0,fmt.Errorf("ไม่พบชื่อคอลัมน์ใน Header")}
	columns,err:=a.mapColumns(ctx,header); if err!=nil{return 0,err}
	var batch [][]interface{}; batch=make([][]interface{},0,a.cfg.BatchSize)
	var total int64
	flush:=func()error {
		if len(batch)==0{return nil}
		n,err:=a.bulkInsert(ctx,columns,batch); if err!=nil{return err}
		total+=n; batch=batch[:0]; return nil
	}
	for {
		if err:=ctx.Err();err!=nil{return total,err}
		record,readErr:=rr.Next()
		if readErr==io.EOF{break}; if readErr!=nil{return total,fmt.Errorf("อ่านข้อมูลแถวถัดไปไม่สำเร็จ: %w",readErr)}
		if len(record)>len(header){return total,fmt.Errorf("พบแถวที่มีจำนวนคอลัมน์มากกว่า Header")}
		values:=make([]interface{},len(columns))
		for i,sourceIndex:=range columns {
			if sourceIndex>=len(record) || strings.TrimSpace(record[sourceIndex])=="" { values[i]=nil } else { values[i]=record[sourceIndex] }
		}
		batch=append(batch,values)
		if len(batch)>=a.cfg.BatchSize {if err:=flush();err!=nil{return total,err}}
	}
	if err:=flush();err!=nil{return total,err}
	return total,nil
}

// mapColumns returns source indexes in the same order as destination SQL columns.
func(a *App)mapColumns(ctx context.Context,header []string)([]int,error) {
	rows,err:=a.db.QueryContext(ctx,`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=@p1 AND TABLE_NAME=@p2 AND COLUMNPROPERTY(OBJECT_ID(QUOTENAME(TABLE_SCHEMA)+'.'+QUOTENAME(TABLE_NAME)), COLUMN_NAME, 'IsIdentity')=0 ORDER BY ORDINAL_POSITION`,a.cfg.Schema,a.cfg.Table)
	if err!=nil{return nil,fmt.Errorf("SQL: อ่านโครงสร้างตารางไม่สำเร็จ: %w",err)}
	defer rows.Close()
	dest:=map[string]string{}
	for rows.Next(){var name string;if err:=rows.Scan(&name);err!=nil{return nil,err};dest[strings.ToLower(name)]=name}
	if err:=rows.Err();err!=nil{return nil,err}
	if len(dest)==0{return nil,fmt.Errorf("SQL: ไม่พบตาราง %s.%s หรือไม่มีคอลัมน์ที่นำเข้าได้",a.cfg.Schema,a.cfg.Table)}
	source:=map[string]int{}
	for i,h:=range header { key:=strings.ToLower(strings.TrimSpace(strings.TrimPrefix(h,"\ufeff"))); if key!="" {source[key]=i} }
	var names []string; var indexes []int
	for key,name:=range dest {if i,ok:=source[key];ok {names=append(names,name);indexes=append(indexes,i)}}
	if len(names)==0{return nil,fmt.Errorf("Header ไม่มีชื่อคอลัมน์ที่ตรงกับ SQL Table")}
	// Keep destination order deterministic by re-reading metadata in ordinal order.
	rows2,err:=a.db.QueryContext(ctx,`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=@p1 AND TABLE_NAME=@p2 AND COLUMNPROPERTY(OBJECT_ID(QUOTENAME(TABLE_SCHEMA)+'.'+QUOTENAME(TABLE_NAME)), COLUMN_NAME, 'IsIdentity')=0 ORDER BY ORDINAL_POSITION`,a.cfg.Schema,a.cfg.Table)
	if err!=nil{return nil,err}; defer rows2.Close()
	indexes=nil
	for rows2.Next(){var name string;if err:=rows2.Scan(&name);err!=nil{return nil,err};if i,ok:=source[strings.ToLower(name)];ok{indexes=append(indexes,i)}}
	if len(indexes)==0{return nil,fmt.Errorf("ไม่พบคอลัมน์ที่จับคู่ได้")}
	return indexes,nil
}

func(a *App)bulkInsert(ctx context.Context,sourceIndexes []int,batch [][]interface{})(int64,error) {
	// Fetch destination column names in the same ordinal order as sourceIndexes.
	rows,err:=a.db.QueryContext(ctx,`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=@p1 AND TABLE_NAME=@p2 AND COLUMNPROPERTY(OBJECT_ID(QUOTENAME(TABLE_SCHEMA)+'.'+QUOTENAME(TABLE_NAME)), COLUMN_NAME, 'IsIdentity')=0 ORDER BY ORDINAL_POSITION`,a.cfg.Schema,a.cfg.Table)
	if err!=nil{return 0,fmt.Errorf("SQL: อ่านชื่อคอลัมน์ไม่สำเร็จ: %w",err)}
	var names []string; for rows.Next(){var n string;if err:=rows.Scan(&n);err!=nil{rows.Close();return 0,err};names=append(names,n)}; rows.Close()
	headerCols,err:=a.mappedDestinationNames(ctx,sourceIndexes); if err!=nil{return 0,err}
	_ = names
	table:=quoteIdentifier(a.cfg.Schema)+"."+quoteIdentifier(a.cfg.Table)
	conn,err:=a.db.Conn(ctx);if err!=nil{return 0,fmt.Errorf("SQL: ขอ connection ไม่สำเร็จ: %w",err)};defer conn.Close()
	var affected int64
	err=conn.Raw(func(dc interface{})error {
		c,ok:=dc.(*mssql.Conn);if !ok{return fmt.Errorf("SQL: driver connection type mismatch")}
		bulk:=c.CreateBulkContext(ctx,table,headerCols)
		for _,row:=range batch {if err:=bulk.AddRow(row);err!=nil{return err};affected++}
		_,err:=bulk.Done();return err
	})
	if err!=nil{return 0,fmt.Errorf("SQL: Bulk Copy ล้มเหลว: %w",err)}
	return affected,nil
}
func(a *App)mappedDestinationNames(ctx context.Context,sourceIndexes []int)([]string,error) {
	rows,err:=a.db.QueryContext(ctx,`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=@p1 AND TABLE_NAME=@p2 AND COLUMNPROPERTY(OBJECT_ID(QUOTENAME(TABLE_SCHEMA)+'.'+QUOTENAME(TABLE_NAME)), COLUMN_NAME, 'IsIdentity')=0 ORDER BY ORDINAL_POSITION`,a.cfg.Schema,a.cfg.Table)
	if err!=nil{return nil,err};defer rows.Close()
	var names []string;for rows.Next(){var n string;if err:=rows.Scan(&n);err!=nil{return nil,err};names=append(names,n)}
	var mapped []string
	for _,idx:=range sourceIndexes { _=idx }
	// sourceIndexes are source indexes; destination names must follow header mapping.
	// Rebuild by matching the header indexes retained by mapColumns is handled in the import pipeline.
	return mapped,fmt.Errorf("internal mapping error: destination mapping is not initialized")
}
func quoteIdentifier(s string)string{return "["+strings.ReplaceAll(s,"]","]]")+"]"}
var _ = filepath.Ext
var _ = sql.ErrNoRows
