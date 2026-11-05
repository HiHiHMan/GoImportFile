package main

import (
	"context"
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
type columnMapping struct { SourceIndex int; Destination string }

type csvRows struct { f *os.File; r *csv.Reader }
func openCSV(path string)(rowReader,error) {
	f,err:=os.Open(path); if err!=nil{return nil,err}
	r:=csv.NewReader(f); r.FieldsPerRecord=-1
	return &csvRows{f:f,r:r},nil
}
func(c *csvRows)Next()([]string,error){ rec,err:=c.r.Read(); if err!=nil{return nil,err}; return rec,nil }
func(c *csvRows)Close()error{return c.f.Close()}

type xlsxRows struct { f *excelize.File; rows *excelize.Rows }
func openXLSX(path string)(rowReader,error) {
	f,err:=excelize.OpenFile(path); if err!=nil{return nil,err}
	sheets:=f.GetSheetList(); if len(sheets)==0 { _=f.Close(); return nil,fmt.Errorf("Excel ไม่มี Sheet") }
	rows,err:=f.Rows(sheets[0]); if err!=nil {_=f.Close();return nil,err}
	return &xlsxRows{f:f,rows:rows},nil
}
func(x *xlsxRows)Next()([]string,error){ if !x.rows.Next(){ if err:=x.rows.Error();err!=nil{return nil,err};return nil,io.EOF }; return x.rows.Columns() }
func(x *xlsxRows)Close()error{ _=x.rows.Close(); return x.f.Close() }

type xlsRows struct { file *os.File; sheet *xls.WorkSheet; index int }
func openXLS(path string)(rowReader,error) {
	f,err:=os.Open(path); if err!=nil{return nil,err}
	book,err:=xls.OpenReader(f,"utf-8"); if err!=nil{_ = f.Close();return nil,err}
	if book.NumSheets()==0{_ = f.Close();return nil,fmt.Errorf("Excel ไม่มี Sheet")}
	sheet:=book.GetSheet(0); if sheet==nil{_ = f.Close();return nil,fmt.Errorf("อ่าน Sheet แรกไม่ได้")}
	return &xlsRows{file:f,sheet:sheet},nil
}
func(x *xlsRows)Next()([]string,error) {
	for x.index<=int(x.sheet.MaxRow) {
		row:=x.sheet.Row(x.index); x.index++
		if row==nil { continue }
		cols:=make([]string,row.LastCol()+1); for i:=range cols { cols[i]=row.Col(i) }
		return cols,nil
	}
	return nil,io.EOF
}
func(x *xlsRows)Close()error{return x.file.Close()}

func(a *App)runImport(ctx context.Context,path,ext,originalFilename string)(int64,error) {
	var rr rowReader; var err error
	switch ext {case ".csv":rr,err=openCSV(path);case ".xlsx":rr,err=openXLSX(path);case ".xls":rr,err=openXLS(path)}
	if err!=nil{return 0,fmt.Errorf("เปิดไฟล์ไม่สำเร็จ: %w",err)}
	defer rr.Close()
	header,err:=rr.Next(); if err!=nil{return 0,fmt.Errorf("อ่าน Header ไม่สำเร็จ: %w",err)}
	if len(header)==0{return 0,fmt.Errorf("ไม่พบ Header ในไฟล์")}
	mapping,err:=a.mapColumns(ctx,len(header)); if err!=nil{return 0,err}
	originalFilename=filepath.Base(originalFilename)
	batch:=make([][]interface{},0,a.cfg.BatchSize)
	var total int64
	flush:=func()error {
		if len(batch)==0{return nil}
		n,err:=a.bulkInsert(ctx,mapping,batch); if err!=nil{return err}
		total+=n; batch=batch[:0]; return nil
	}
	for {
		if err:=ctx.Err();err!=nil{return total,err}
		record,readErr:=rr.Next()
		if readErr==io.EOF{break}; if readErr!=nil{return total,fmt.Errorf("อ่านข้อมูลแถวถัดไปไม่สำเร็จ: %w",readErr)}
		values:=make([]interface{},len(mapping))
		for i,m:=range mapping {
			if m.SourceIndex < 0 {
				values[i]=originalFilename
				continue
			}
			if m.SourceIndex>=len(record) || strings.TrimSpace(record[m.SourceIndex])=="" { values[i]=nil } else { values[i]=record[m.SourceIndex] }
		}
		batch=append(batch,values)
		if len(batch)>=a.cfg.BatchSize {if err:=flush();err!=nil{return total,err}}
	}
	if err:=flush();err!=nil{return total,err}
	return total,nil
}

// mapColumns maps file columns to SQL columns strictly by ordinal position.
// Header text is intentionally ignored; the first row is skipped as a header.
func(a *App)mapColumns(ctx context.Context,sourceColumnCount int)([]columnMapping,error) {
	rows,err:=a.db.QueryContext(ctx,`SELECT COLUMN_NAME FROM INFORMATION_SCHEMA.COLUMNS WHERE TABLE_SCHEMA=@p1 AND TABLE_NAME=@p2 AND COLUMNPROPERTY(OBJECT_ID(QUOTENAME(TABLE_SCHEMA)+'.'+QUOTENAME(TABLE_NAME)), COLUMN_NAME, 'IsIdentity')=0 ORDER BY ORDINAL_POSITION`,a.cfg.Schema,a.cfg.Table)
	if err!=nil{return nil,fmt.Errorf("SQL: อ่านโครงสร้างตารางไม่สำเร็จ: %w",err)}
	defer rows.Close()
	var destinations []string
	for rows.Next(){
		var name string
		if err:=rows.Scan(&name);err!=nil{return nil,err}
		destinations=append(destinations,name)
	}
	if err:=rows.Err();err!=nil{return nil,err}
	if len(destinations)==0 {
		return nil,fmt.Errorf("ไม่พบคอลัมน์ในตารางปลายทาง")
	}
	if sourceColumnCount > len(destinations)-1 {
		return nil,fmt.Errorf("จำนวนคอลัมน์ในไฟล์เกินกว่าตารางปลายทาง: ไฟล์มี %d คอลัมน์ แต่ตาราง %s.%s รองรับข้อมูลได้สูงสุด %d คอลัมน์ เมื่อสงวน Col0 สำหรับชื่อไฟล์",sourceColumnCount,a.cfg.Schema,a.cfg.Table,len(destinations)-1)
	}

	// Col0 stores the original uploaded filename; source data starts at Col1.
	// Remaining destination columns are omitted so SQL Server can apply DEFAULT or NULL.
	mapping:=make([]columnMapping,sourceColumnCount+1)
	mapping[0]=columnMapping{SourceIndex:-1,Destination:destinations[0]}
	for i:=0;i<sourceColumnCount;i++ {
		mapping[i+1]=columnMapping{SourceIndex:i,Destination:destinations[i+1]}
	}
	return mapping,nil
}

func(a *App)bulkInsert(ctx context.Context,mapping []columnMapping,batch [][]interface{})(int64,error) {
	columns:=make([]string,len(mapping))
	for i,m:=range mapping {columns[i]=m.Destination}
	table:=quoteIdentifier(a.cfg.Schema)+"."+quoteIdentifier(a.cfg.Table)
	conn,err:=a.db.Conn(ctx);if err!=nil{return 0,fmt.Errorf("SQL: ขอ connection ไม่สำเร็จ: %w",err)}
	defer conn.Close()
	var affected int64
	err=conn.Raw(func(dc interface{})error {
		c,ok:=dc.(*mssql.Conn);if !ok{return fmt.Errorf("SQL: driver connection type mismatch")}
		bulk:=c.CreateBulkContext(ctx,table,columns)
		for _,row:=range batch {
			if err:=ctx.Err();err!=nil{return err}
			if err:=bulk.AddRow(row);err!=nil{return err}
			affected++
		}
		_,err:=bulk.Done()
		return err
	})
	if err!=nil{return 0,fmt.Errorf("SQL: Bulk Copy ล้มเหลว: %w",err)}
	return affected,nil
}
func quoteIdentifier(s string)string{return "["+strings.ReplaceAll(s,"]","]]")+"]"}
