package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/gedex/inflector"
	_ "github.com/lib/pq"
)

func getTableName(db *sql.DB) ([]string, error) {
	query := `select relname as TABLE_NAME from pg_stat_user_tables`

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}

	tableNames := []string{}
	for rows.Next() {
		var tableName string
		err = rows.Scan(&tableName)
		if err != nil {
			return nil, err
		}

		tableNames = append(tableNames, tableName)
	}

	return tableNames, nil
}

func getPrimaryKeys(tableName string, db *sql.DB) (map[string]bool, error) {
	query :=
		`
    select
    ccu.column_name as COLUMN_NAME
    from
      information_schema.table_constraints tc
      ,information_schema.constraint_column_usage ccu
    where
      tc.table_name='` + tableName + `'
      and
      tc.constraint_type='PRIMARY KEY'
      and
      tc.table_catalog=ccu.table_catalog
      and
      tc.table_schema=ccu.table_schema
      and
      tc.table_name=ccu.table_name
      and
      tc.constraint_name=ccu.constraint_name
    `

	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}

	primaryKeys := map[string]bool{}
	for rows.Next() {
		var columnName string
		err = rows.Scan(&columnName)
		if err != nil {
			return nil, err
		}

		primaryKeys[columnName] = true
	}

	return primaryKeys, nil
}

func genModel(tableNames []string, outPath string, db *sql.DB) error {
	for _, tableName := range tableNames {

		primaryKeys, err := getPrimaryKeys(tableName, db)
		if err != nil {
			return err
		}

		query :=
			`
      select column_name, data_type, COALESCE(column_default, '') as column_default, is_nullable
      from information_schema.columns
      where
        table_name='` + tableName + `'
      order by
        ordinal_position;
      `

		rows, err := db.Query(query)
		if err != nil {
			return err
		}

		var gormStr string
		var needTimePackage bool
		for rows.Next() {
			var (
				columnName    string
				dataType      string
				columnDefault string
				isNullable    string
			)

			err = rows.Scan(&columnName, &dataType, &columnDefault, &isNullable)
			if err != nil {
				return err
			}

			json := genJSON(columnName, columnDefault, primaryKeys)

			if dataType == "timestamp with time zone" {
				needTimePackage = true
			}

			// If have to use pointer
			if dataType == "timestamp with time zone" && isNullable == "YES" {
				hasNullRecords, err := hasNullRecords(tableName, columnName, db)
				if err != nil {
					return err
				}

				if hasNullRecords {
					dataType = "*time.Time"
				}
			}

			m := gormColName(columnName) + " " + gormDataType(dataType) + " `" + json + "`\n"
			gormStr += m

			isInfered, infColName := inferORM(columnName)

			// Add belongs_to relation
			if isInfered == true {
				json := genJSON(strings.ToLower(infColName), "", nil)
				comment := "// This line is infered from column name \"" + columnName + "\"."
				infColName = gormColName(infColName)

				m := infColName + " *" + infColName + " `" + json + "` " + comment + "\n"
				gormStr += m
			}
		}

		var importPackage string
		if needTimePackage {
			importPackage = "import \"time\"\n\n"
		} else {
			importPackage = ""
		}

		gormStr = "package models\n\n" + importPackage + "type " + gormTableName(tableName) + " struct {\n" + gormStr + "}\n"

		file, err := os.Create(outPath + inflector.Singularize(tableName) + `.go`)
		if err != nil {
			return err
		}
		defer file.Close()
		file.Write(([]byte)(gormStr))
	}

	err := exec.Command("gofmt", "-w", outPath).Run()
	if err != nil {
		return err
	}

	return nil
}

// Infer belongs_to Relation from column's name
func inferORM(s string) (bool, string) {
	s = strings.ToLower(s)
	ss := strings.Split(s, "_")

	const (
		id = "id"
	)

	newSS := []string{}
	var containsID bool = false
	for _, word := range ss {
		if word == id {
			containsID = true
			continue
		}

		newSS = append(newSS, word)
	}

	if containsID == false || len(newSS) == 0 {
		return false, ""
	}

	infColName := strings.Join(newSS, "_")
	return true, infColName
}

// Generate json
func genJSON(columnName, columnDefault string, primaryKeys map[string]bool) (json string) {
	json = "json:\"" + columnName + "\""

	if primaryKeys[columnName] == true {
		p := "gorm:\"primary_key;AUTO_INCREMENT\" "
		json = p + json
	}

	if columnDefault != "" && !strings.Contains(columnDefault, "nextval") {
		d := " sql:\"DEFAULT:" + columnDefault + "\""
		json += d
	}

	return
}

func hasNullRecords(tableName string, columnName string, db *sql.DB) (bool, error) {
	query := `SELECT COUNT(*) FROM ` + tableName + ` WHERE ` + columnName + ` IS NULL;`

	var count string

	err := db.QueryRow(query).Scan(&count)
	if err != nil {
		return false, err
	}

	val, _ := strconv.ParseInt(count, 10, 64)

	if val > 0 {
		return true, nil
	}

	return false, nil
}

// Singlarlize table name and upper initial character
func gormTableName(s string) string {
	s = strings.ToLower(s)
	s = inflector.Singularize(s)
	return strings.Title(s)
}

// Ex: facebook_uid → FacebookUID
func gormColName(s string) string {
	s = strings.ToLower(s)
	ss := strings.Split(s, "_")
	const (
		id  = "id"
		url = "url"
	)
	for i, word := range ss {
		if strings.Contains(word, id) {
			word = strings.Replace(word, id, "ID", -1)
		}
		if strings.Contains(word, url) {
			word = strings.Replace(word, url, "URL", -1)
		}
		ss[i] = strings.Title(word)
	}
	return strings.Join(ss, "")
}

func gormDataType(s string) string {
	switch s {
	case "integer":
		return "uint"
	case "numeric":
		return "float64"
	case "character varying", "text":
		return "string"
	case "boolean":
		return "bool"
	case "timestamp with time zone":
		return "time.Time"
	default:
		return s
	}
}

func main() {
	var url string
	var dir string
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: Generate gorm model structs from PostgreSQL database schema.\n")
		flag.PrintDefaults() // Print usage of options
	}
	flag.StringVar(&url, "url", "", "Set DB URL")
	flag.StringVar(&url, "u", "", "Set DB URL")
	flag.StringVar(&dir, "dir", "./", "Set output path")
	flag.StringVar(&dir, "d", "./", "Set output path")
	flag.Parse()

	if url != "" {
		fmt.Printf("Connecting to database...\n")

		db, err := sql.Open("postgres", url)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer db.Close()

		tables, err := getTableName(db)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

		fmt.Println("Generating gorm from tables below...")
		for _, tableName := range tables {
			fmt.Printf("Table name: %s\n", tableName)
		}

		err = genModel(tables, dir+"/", db)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	} else {
		fmt.Fprintf(os.Stderr, "Usage: Generate gorm model structs from PostgreSQL database schema.\n")
		flag.PrintDefaults() // Print usage of options
		os.Exit(1)
	}
}
