package restore

type PartitionReport struct {
	Partition
	Status         string `json:"status"`
	ReadbackSHA256 string `json:"readback_sha256,omitempty"`
}
type Report struct {
	Schema             int               `json:"schema"`
	Plan               Plan              `json:"plan"`
	Status             string            `json:"status"`
	Stage              string            `json:"stage"`
	Started            string            `json:"started_utc"`
	Finished           string            `json:"finished_utc,omitempty"`
	PartialDataAllowed bool              `json:"partial_data_allowed"`
	EnvironmentSHA256  string            `json:"previous_environment_sha256,omitempty"`
	Partitions         []PartitionReport `json:"partitions"`
	StockBootVerified  bool              `json:"stock_boot_environment_verified"`
	Error              string            `json:"error,omitempty"`
}
