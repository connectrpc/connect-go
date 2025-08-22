package connect

type ExperimentalFeatures struct {
	AllowBidiStreamOverHTTP11 bool
}

var DefaultExperimentalFeatures = ExperimentalFeatures{
	AllowBidiStreamOverHTTP11: true,
}
