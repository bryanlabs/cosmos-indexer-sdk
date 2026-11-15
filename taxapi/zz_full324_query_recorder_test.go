//go:build ops818harness

package taxapi

import (
 "context"
 "encoding/json"
 "os"
 "time"
 "gorm.io/gorm"
 "gorm.io/gorm/logger"
)

type nextReadQuery struct {SQL string;Milliseconds int64;Rows int64;Error string}
type nextReadRecorder struct {logger.Interface; Output string; Queries []nextReadQuery}
func (r *nextReadRecorder) Trace(_ context.Context,start time.Time,query func()(string,int64),err error) {
 sql,rows:=query();record:=nextReadQuery{SQL:sql,Milliseconds:time.Since(start).Milliseconds(),Rows:rows}
 if err!=nil {record.Error=err.Error()};r.Queries=append(r.Queries,record)
 data,_:=json.Marshal(r.Queries);_=os.WriteFile(r.Output,data,0600)
}
func recordNextReadDB(db *gorm.DB,path string)*gorm.DB {
 r:=&nextReadRecorder{Interface:logger.Default.LogMode(logger.Silent),Output:path}
 return db.Session(&gorm.Session{Logger:r})
}
