package version

const (
	Name    = "Prosiak"
	Version = "1.0"
)

func FullName() string {
	return Name + " " + Version
}
