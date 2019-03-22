package river

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/juju/errors"
	"elastic"
	"github.com/siddontang/go-mysql/canal"
	"github.com/siddontang/go-mysql/client"
	"github.com/siddontang/go-mysql/mysql"
	"github.com/siddontang/go-mysql/replication"
	"github.com/siddontang/go-mysql/schema"
	log "github.com/sirupsen/logrus"
	"reflect"
	"strings"
	"time"
	"regexp"
)
const (
	syncInsertDoc = iota
	syncDeleteDoc
	syncUpdateDoc
)

const (
	fieldTypeList = "list"
	// for the mysql int type to es date type
	// set the [rule.field] created_time = ",date"
	createtime= ",date"
	diningtime= ",date"
	fieldTypeDate = "date"
)

type posSaver struct {
	pos   mysql.Position
	force bool
}

type eventHandler struct {
	r *River
}
var (
	expCreateTable1 = regexp.MustCompile("(?i)^CREATE\\s+TABLE\\s+IF\\s+NOT\\s+EXISTS\\s.*?`{0,1}(.*?)`{0,1}\\s.*")
	expCreateTable2 = regexp.MustCompile("(?i)^CREATE\\s+TABLE\\s+.*?`{0,1}(.*?)`{0,1}\\s.*")
	expDropTable   = regexp.MustCompile("(?i)^DROP\\s+TABLE\\s+.*?`{0,1}(.*?)`{0,1}\\s.*")
	expAlterTable  = regexp.MustCompile("(?i)^ALTER\\s+TABLE\\s+.*?`{0,1}(.*?)`{0,1}\\.{0,1}`{0,1}([^`\\.]+?)`{0,1}\\s.*")
	expRenameTable = regexp.MustCompile("(?i)^RENAME\\s+TABLE.*TO\\s+.*?`{0,1}(.*?)`{0,1}\\.{0,1}`{0,1}([^`\\.]+?)`{0,1}$")
)
func (h *eventHandler) OnRotate(e *replication.RotateEvent) error {
	pos := mysql.Position{
		string(e.NextLogName),
		uint32(e.Position),
	}
	h.r.syncCh <- posSaver{pos, true}

	return h.r.ctx.Err()
}
func (h *eventHandler) OnDDL(nextPos mysql.Position, e *replication.QueryEvent) error {
	if len(h.r.c.ESAddr)>0{

	}else{
		var sdatabase string
		var inSchema string
		var dosql string
		for _, s := range h.r.c.Sources {
			sdatabase = s.Schema
		}
		inSchema=fmt.Sprintf("%s",e.Schema)
		if inSchema==sdatabase && len(e.Query)>5{
			dosql=fmt.Sprintf("%s",e.Query)
			dosql=strings.ToLower(dosql)
			conn, _ := client.Connect(h.r.c.MytoAddr, h.r.c.MytoUser, h.r.c.MytoPassword, sdatabase)
			res, err := conn.Execute(dosql)
			if err != nil {
				log.Warnf("-------------------------%s-----------------",err,res)
			}
			mb := checkRenameTable(e)
			if mb != nil {
				if(len(mb)>2){
					mb[1] = mb[2]
				}
				tablename:=fmt.Sprintf("%s",mb[1])
				dosql=strings.ToLower(dosql)
				if strings.Index(dosql,"create table")>=0{
					err := h.r.newRule(sdatabase, tablename)
					if err != nil {
						log.Warnf("---%s------%s------%s------",dosql,mb[1],err)
					}
				}
				h.r.canal.ClearTableCache(e.Schema, mb[1])
				log.Infof("table structure changed, clear table cache: %s.%s\n", e.Schema, mb[1])
				rule, ok := h.r.rules[ruleKey(sdatabase, tablename)]
				if !ok {
					return nil
				}
				rule.TableInfo,err = h.r.canal.GetTable(sdatabase, tablename); 
				if err != nil {
					log.Warnf("---%s------%s-------%s-----",dosql,tablename,err)
				}else{
					h.r.rules[ruleKey(sdatabase, tablename)]=rule
				}
			}else{
				log.Warnf("-----------------%v-----------------",mb)
			}
			
		}
	}
	h.r.syncCh <- posSaver{nextPos, true}
	return h.r.ctx.Err()
}

func (h *eventHandler) OnXID(nextPos mysql.Position) error {
	h.r.syncCh <- posSaver{nextPos, false}
	return h.r.ctx.Err()
}

func (h *eventHandler) OnRow(e *canal.RowsEvent) error {
	rule, ok := h.r.rules[ruleKey(e.Table.Schema, e.Table.Name)]
	if !ok {
	      return nil	
	}
	var reqs []*elastic.BulkRequest
	var err error
	switch e.Action {
	case canal.InsertAction:
		reqs, err = h.r.makeInsertRequest(rule, e.Rows)
	case canal.DeleteAction:
		reqs, err = h.r.makeDeleteRequest(rule, e.Rows)
	case canal.UpdateAction:
		reqs, err = h.r.makeUpdateRequest(rule, e.Rows)
	default:
		err = errors.Errorf("invalid rows action %s", e.Action)
	}
	if err != nil {
		h.r.cancel()
		return errors.Errorf("make %s ES request err %v, close sync", e.Action, err)
	}

	h.r.syncCh <- reqs

	return h.r.ctx.Err()
}

func (h *eventHandler) OnGTID(gtid mysql.GTIDSet) error {
	return nil
}
func (h *eventHandler) OnPosSynced(pos mysql.Position, force bool) error {
	return nil
}

func (h *eventHandler) String() string {
	return "ESRiverEventHandler"
}
func (r *River) syncLoop() {
	bulkSize := r.c.BulkSize
	if bulkSize == 0 {
		bulkSize = 128
	}

	interval := r.c.FlushBulkTime.Duration
	if interval == 0 {
		interval = 200 * time.Millisecond
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer r.wg.Done()
	logtime:=time.Now()
	lastSavedTime := time.Now()
	reqs := make([]*elastic.BulkRequest, 0, 1024)
	trynumt:=0
	var pos mysql.Position
	for {
		needFlush := false
		needSavePos := false
		nowlog := time.Now()
		if nowlog.Sub(logtime)>1800*time.Second {
			if nowlog.Sub(lastSavedTime)>1800*time.Second && trynumt<3{
				r.canal.Tryrun()
				logtime=nowlog
				trynumt=trynumt+1
			}else if nowlog.Sub(lastSavedTime)>1800*time.Second{
				trynumt=trynumt
			}else{
				trynumt=0
			}
		}
		select {
		case v := <-r.syncCh:
			switch v := v.(type) {
			case posSaver:
				now := time.Now()
				if v.force || now.Sub(lastSavedTime) > 3*time.Second {
					lastSavedTime = now
					needFlush = true
					needSavePos = true
					pos = v.pos
				}
			case []*elastic.BulkRequest:
				reqs = append(reqs, v...)
				needFlush = len(reqs) >= bulkSize
			}
		case <-ticker.C:
			needFlush = true
		case <-r.ctx.Done():
			return
		}
		//同步到mysql判断
		if len(r.c.ESAddr)>0{
			if needFlush {
				// TODO: retry some times?
				//内存中的数据全部写入清除
				if err := r.doBulk(reqs); err != nil {
					log.Errorf("do ES bulk err %v, close sync", err)
					r.cancel()
					return
				}
				reqs = reqs[0:0]
			}
		}
		if needSavePos {
			if err := r.master.Save(pos); err != nil {
				log.Errorf("save sync position %s err %v, close sync", pos, err)
				r.cancel()
				return
			}
		}
	}
}

// for insert and delete
func (r *River) makeRequest(rule *Rule, action string, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	reqs := make([]*elastic.BulkRequest, 0, len(rows))
	var dosql string
	for _, values := range rows {
		if len(r.c.ESAddr)>0{
			id, err := r.getDocID(rule, values)
			if err != nil {
				return nil, errors.Trace(err)
			}

			parentID := ""
			if len(rule.Parent) > 0 {
				if parentID, err = r.getParentID(rule, values, rule.Parent); err != nil {
					return nil, errors.Trace(err)
				}
			}
			req := &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: id, Parent: parentID}
			if action == canal.DeleteAction {
				if rule.TableInfo.Name == "alp_merchant_order" || rule.TableInfo.Name == "alp_delivery_detial" || rule.TableInfo.Name == "alp_merchant_order_item"{
					continue
				}else{
					req.Action = elastic.ActionDelete
					r.st.DeleteNum.Add(1)
				}
			} else {
				r.makeInsertReqData(req, rule, values)
				r.st.InsertNum.Add(1)
			}
			reqs = append(reqs, req)
		}else{
			if action == canal.DeleteAction {
				dosql="delete from "
				dosql+=rule.TableInfo.Name
				dosql+=" where "
				dosql+=rule.TableInfo.Columns[0].Name
				dosql+="="
				var valuest string
				if values[0] !=nil{
					var cbyte string
					cbyte=fmt.Sprintf("%T",values[0])
					if cbyte=="int" || cbyte=="int64" || cbyte=="int32"  || cbyte=="int8" || cbyte=="int16" {
						valuest=fmt.Sprintf("%d",values[0])
					}else if cbyte=="float64" || cbyte=="float" || cbyte=="float32" || cbyte=="float8" {
						valuest=fmt.Sprintf("%.2f",values[0])
					}else{
						valuest=fmt.Sprintf("%s",values[0])
					}
				}else{
					return nil, errors.Errorf("删除记录无条件")
				}
				dosql+="'"+valuest+"'"
			} else if action == canal.InsertAction{
				var valuest string
				dosql="insert into "
				dosql+=rule.TableInfo.Name
				keys := make([]string, 0, len(rule.TableInfo.Columns))
				vals:=  make([]string, 0, len(rule.TableInfo.Columns))
				for j,c:=range rule.TableInfo.Columns {
					if !rule.CheckFilter(c.Name) {
						continue
					}
					if len(values)>j{
						if values[j] !=nil{
							var cbyte string
							cbyte=fmt.Sprintf("%T",values[j])
							if cbyte=="int" || cbyte=="int64" || cbyte=="int32" || cbyte=="int8" || cbyte=="int16"{
								valuest=fmt.Sprintf("%d",values[j])
							}else if cbyte=="float64" || cbyte=="float8" || cbyte=="float32" {
								valuest=fmt.Sprintf("%.2f",values[j])
							}else{
								valuest=fmt.Sprintf("%s",values[j])
							}
						}else{
							valuest=""
						}
					}else{
						valuest=""
					}
					if values[j] !=nil{
						keys=append(keys,c.Name)
						valuest="'"+valuest+"'"
						vals=append(vals,valuest)
					}
				}
				dosql+="("+strings.Join(keys,",")+")values("
				dosql+=strings.Join(vals,",")+")"
			}
			dosql=strings.Replace(dosql, "\\", "\\\\", -1)
			//log.Warnf("---------%s--------------------------",dosql)
			for{
				if r.rdosql>r.c.Mytomaxcon{
					time.Sleep(time.Duration(1)*time.Second)
					break
				}else{
					break
				}
			}
			go r.runsql(dosql)
		}
		
	}

	return reqs, nil
}
func (r *River) runsql(dosql string){
	var err error
	var sdatabase string
	for _, s := range r.c.Sources {
		sdatabase = s.Schema
	}
	cn, _:=client.Connect(r.c.MytoAddr, r.c.MytoUser, r.c.MytoPassword, sdatabase)
	r.rdosql=r.rdosql+1
	res, err := cn.Execute(dosql)
	r.rdosql=r.rdosql-1
	if err != nil {
		log.Warnf("---------%s-----------%s-----------------","Warnf:wait for next do",err,res)
		time.Sleep(500*time.Nanosecond)
		go r.wrunsql(dosql)
	}
	if(r.rdosql<10){
		r.rdosql=0;
	}
	cn.Close()
}
//协程先后问题
func (r *River) wrunsql(dosql string){
	var err error
	var sdatabase string
	for _, s := range r.c.Sources {
		sdatabase = s.Schema
	}
	cn, _:=client.Connect(r.c.MytoAddr, r.c.MytoUser, r.c.MytoPassword, sdatabase)
	res, err := cn.Execute(dosql)
	if err != nil {
		log.Warnf("error:---------%s-----------%s-----------------",dosql,err,res)
	}
	cn.Close()
}
func (r *River) makeInsertRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {

	return r.makeRequest(rule, canal.InsertAction, rows)
}

func (r *River) makeDeleteRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
		
	return r.makeRequest(rule, canal.DeleteAction, rows)
		
}

func (r *River) makeUpdateRequest(rule *Rule, rows [][]interface{}) ([]*elastic.BulkRequest, error) {
	if len(rows)%2 != 0 {
		return nil, errors.Errorf("invalid update rows event, must have 2x rows, but %d", len(rows))
	}
	var dosql string
	reqs := make([]*elastic.BulkRequest, 0, len(rows))
	for i := 0; i < len(rows); i += 2 {
		if len(r.c.ESAddr)>0{
		beforeID, err := r.getDocID(rule, rows[i])
			if err != nil {
				return nil, errors.Trace(err)
			}

			afterID, err := r.getDocID(rule, rows[i+1])

			if err != nil {
				return nil, errors.Trace(err)
			}

			beforeParentID, afterParentID := "", ""
			if len(rule.Parent) > 0 {
				if beforeParentID, err = r.getParentID(rule, rows[i], rule.Parent); err != nil {
					return nil, errors.Trace(err)
				}
				if afterParentID, err = r.getParentID(rule, rows[i+1], rule.Parent); err != nil {
					return nil, errors.Trace(err)
				}
			}
			req := &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: beforeID, Parent: beforeParentID}
			if beforeID != afterID || beforeParentID != afterParentID {
				req.Action = elastic.ActionDelete
				reqs = append(reqs, req)

				req = &elastic.BulkRequest{Index: rule.Index, Type: rule.Type, ID: afterID, Parent: afterParentID}
				r.makeInsertReqData(req, rule, rows[i+1])

				r.st.DeleteNum.Add(1)
				r.st.InsertNum.Add(1)
			} else {
				r.makeUpdateReqData(req, rule, rows[i], rows[i+1])
				r.st.UpdateNum.Add(1)
			}

			reqs = append(reqs, req)
		}else{
			dosql="update "
			dosql+=rule.TableInfo.Name
			dosql+=" set "
			var valuest string
			var wherev string
			for j,c:=range rule.TableInfo.Columns {
				if !rule.CheckFilter(c.Name) {
					continue
				}
				if rows[i+1][j] !=nil{
					var cbyte string
					cbyte=fmt.Sprintf("%T",rows[i+1][j])
					//log.Warnf("-----------%s--------------%s-----------------",c.Name,cbyte)
					if cbyte=="int" || cbyte=="int64" || cbyte=="int32" || cbyte=="int8" || cbyte=="int16" {
						valuest=fmt.Sprintf("%d",rows[i+1][j])
					}else if cbyte=="float64" || cbyte=="float" || cbyte=="float32" || cbyte=="float8" {
						valuest=fmt.Sprintf("%.2f",rows[i+1][j])
					}else{
						valuest=fmt.Sprintf("%s",rows[i+1][j])
					}
					if(j==0){
						wherev=valuest
					}
				}else{
					valuest=""
				}
				if rows[i+1][j] !=nil{
					dosql+=rule.TableInfo.Columns[j].Name
					dosql+="="
					dosql+="'"+valuest+"'"
					dosql+=","
				}
			}
			var r = []rune(dosql)
			var sublen=len(r)-1
			dosql = string(r[0 : sublen])
			dosql += " where "
			dosql += rule.TableInfo.Columns[0].Name
			dosql += "="
			dosql += "'"+wherev+"'"
		}
		if len(r.c.ESAddr)>0{
		}else{
			for{
				if r.rdosql>r.c.Mytomaxcon{
					time.Sleep(time.Duration(1)*time.Second)
					break
				}else{
					break
				}
			}
			dosql=strings.Replace(dosql, "\\", "\\\\", -1)
			go r.runsql(dosql)
		}
	}
	return reqs, nil
}
func (r *River) makeReqColumnData(col *schema.TableColumn, value interface{}) interface{} {
	switch col.Type {
	case schema.TYPE_ENUM:
		switch value := value.(type) {
		case int64:
			// for binlog, ENUM may be int64, but for dump, enum is string
			eNum := value - 1
			if eNum < 0 || eNum >= int64(len(col.EnumValues)) {
				// we insert invalid enum value before, so return empty
				log.Warnf("invalid binlog enum index %d, for enum %v", eNum, col.EnumValues)
				return ""
			}

			return col.EnumValues[eNum]
		}
	case schema.TYPE_SET:
		switch value := value.(type) {
		case int64:
			// for binlog, SET may be int64, but for dump, SET is string
			bitmask := value
			sets := make([]string, 0, len(col.SetValues))
			for i, s := range col.SetValues {
				if bitmask&int64(1<<uint(i)) > 0 {
					sets = append(sets, s)
				}
			}
			return strings.Join(sets, ",")
		}
	case schema.TYPE_BIT:
		switch value := value.(type) {
		case string:
			// for binlog, BIT is int64, but for dump, BIT is string
			// for dump 0x01 is for 1, \0 is for 0
			if value == "\x01" {
				return int64(1)
			}

			return int64(0)
		}
	case schema.TYPE_STRING:
		switch value := value.(type) {
		case []byte:
			return string(value[:])
		}
	case schema.TYPE_JSON:
		var f interface{}
		var err error
		switch v := value.(type) {
		case string:
			err = json.Unmarshal([]byte(v), &f)
		case []byte:
			err = json.Unmarshal(v, &f)
		}
		if err == nil && f != nil {
			return f
		}
	case schema.TYPE_DATETIME, schema.TYPE_TIMESTAMP:
		switch v := value.(type) {
		case string:
			local1, err1 := time.LoadLocation("")
			if err1 != nil {
				fmt.Println(err1)
			}
			vt, _ := time.ParseInLocation(mysql.TimeFormat, string(v), local1)
			return vt.Format(time.RFC3339)
		}
	}

	return value
}

func (r *River) getFieldParts(k string, v string) (string, string, string) {
	composedField := strings.Split(v, ",")

	mysql := k
	elastic := composedField[0]
	fieldType := ""

	if 0 == len(elastic) {
		elastic = mysql
	}
	if 2 == len(composedField) {
		fieldType = composedField[1]
	}

	return mysql, elastic, fieldType
}

func (r *River) makeInsertReqData(req *elastic.BulkRequest, rule *Rule, values []interface{}) {
	req.Data = make(map[string]interface{}, len(values))
	//fmt.Printf("%#v\n", rule.TableInfo)
	req.Action = elastic.ActionIndex
	for i, c := range rule.TableInfo.Columns {
		if !rule.CheckFilter(c.Name) {
			continue
		}
		mapped := false
		for k, v := range rule.FieldMapping {
			mysql, elastic, fieldType := r.getFieldParts(k, v)
			if mysql == c.Name {
				mapped = true
				req.Data[elastic] = r.getFieldValue(&c, fieldType, values[i])
			}
		}
		if mapped == false {
			req.Data[c.Name] = r.makeReqColumnData(&c, values[i])
		}
	}
	
}

func (r *River) makeUpdateReqData(req *elastic.BulkRequest, rule *Rule,
	beforeValues []interface{}, afterValues []interface{}) {
	req.Data = make(map[string]interface{}, len(beforeValues))	
	// maybe dangerous if something wrong delete before?
	req.Action = elastic.ActionUpdate
	for i, c := range rule.TableInfo.Columns {
		mapped := false
		if !rule.CheckFilter(c.Name) {
			continue
		}
		if reflect.DeepEqual(beforeValues[i], afterValues[i]) {
			//nothing changed
			continue
		}
		for k, v := range rule.FieldMapping {

			mysql, elastic, fieldType := r.getFieldParts(k, v)
			if mysql == c.Name {
				mapped = true
				req.Data[elastic] = r.getFieldValue(&c, fieldType, afterValues[i])

			}
		}
		if mapped == false {
			req.Data[c.Name] = r.makeReqColumnData(&c, afterValues[i])
		}
	}
}

// If id in toml file is none, get primary keys in one row and format them into a string, and PK must not be nil
// Else get the ID's column in one row and format them into a string
func (r *River) getDocID(rule *Rule, row []interface{}) (string, error) {
	var (
		ids []interface{}
		err error
	)
	if rule.ID == nil {
		ids, err = canal.GetPKValues(rule.TableInfo, row)
		if err != nil {
			return "", err
		}
	} else {
		ids = make([]interface{}, 0, len(rule.ID))
		for _, column := range rule.ID {
			value, err := canal.GetColumnValue(rule.TableInfo, column, row)
			if err != nil {
				return "", err
			}
			ids = append(ids, value)
		}
	}

	var buf bytes.Buffer

	sep := ""
	for i, value := range ids {
		if value == nil {
			return "", errors.Errorf("The %ds id or PK value is nil", i)
		}

		buf.WriteString(fmt.Sprintf("%s%v", sep, value))
		sep = ":"
	}

	return buf.String(), nil
}

func (r *River) getParentID(rule *Rule, row []interface{}, columnName string) (string, error) {
	index := rule.TableInfo.FindColumn(columnName)
	if index < 0 {
		return "", errors.Errorf("parent id not found %s(%s)", rule.TableInfo.Name, columnName)
	}

	return fmt.Sprint(row[index]), nil
}

func (r *River) doBulk(reqs []*elastic.BulkRequest) error {
	if len(reqs) == 0 {
		return nil
	}

	if resp, err := r.es.Bulk(reqs); err != nil {
		log.Errorf("sync docs err %v after binlog %s", err, r.canal.SyncedPosition())
		return errors.Trace(err)
	} else if resp.Code/100 == 2 || resp.Errors {
		for i := 0; i < len(resp.Items); i++ {
			for action, item := range resp.Items[i] {
				if len(item.Error) > 0 {
					log.Errorf("%s index: %s, type: %s, id: %s, status: %d, error: %s",
						action, item.Index, item.Type, item.ID, item.Status, item.Error)
				}
			}
		}
	}

	return nil
}

// get mysql field value and convert it to specific value to es
func (r *River) getFieldValue(col *schema.TableColumn, fieldType string, value interface{}) interface{} {
	var fieldValue interface{}
	switch fieldType {
	case fieldTypeList:
		v := r.makeReqColumnData(col, value)
		if str, ok := v.(string); ok {
			fieldValue = strings.Split(str, ",")
		} else {
			fieldValue = v
		}

	case fieldTypeDate:
		if col.Type == schema.TYPE_NUMBER {
			col.Type = schema.TYPE_DATETIME

			v := reflect.ValueOf(value)
			switch v.Kind() {
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				fieldValue = r.makeReqColumnData(col, time.Unix(v.Int(), 0).Format(mysql.TimeFormat))
			}
		}
	}

	if fieldValue == nil {
		fieldValue = r.makeReqColumnData(col, value)
	}
	return fieldValue
}
func checkRenameTable(e *replication.QueryEvent) [][]byte {
	var mb = [][]byte{}
	if mb = expCreateTable1.FindSubmatch(e.Query);mb !=nil{
		return mb
	}else if mb = expCreateTable2.FindSubmatch(e.Query);mb !=nil{
		return mb
	}
	if mb = expDropTable.FindSubmatch(e.Query);mb !=nil{
		return mb
	}
	if mb = expAlterTable.FindSubmatch(e.Query); mb != nil {
		return mb
	}
	if mb = expRenameTable.FindSubmatch(e.Query); mb != nil {
		return mb
	}
	return nil
}
