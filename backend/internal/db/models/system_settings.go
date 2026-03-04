package models

type SystemSettings struct {
	Key   string `db:"key" json:"key"`
	Value string `db:"value" json:"value"`
}
