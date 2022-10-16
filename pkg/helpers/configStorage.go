package helpers

import (
	"io/ioutil"
	"path"
	"sync"

	"encoding/json"

	log "github.com/sirupsen/logrus"
	"gopkg.in/yaml.v2"
)

type ConfigFile struct {
	Filename  string `yaml:"ignore" json:"ignore"`
	AppConfig interface{}

	mtx sync.Mutex
}

func NewAppData(confFilename string) (*ConfigFile, error) {
	c := &ConfigFile{
		Filename: confFilename,
	}

	return c, nil
}

func (c *ConfigFile) Lock() {
	c.mtx.Lock()
}

func (c *ConfigFile) Unlock() {
	c.mtx.Unlock()
}

func (c *ConfigFile) Save() error {
	return c.SaveExplicit(c.AppConfig)
}

func (c *ConfigFile) SaveExplicit(a interface{}) error {
	var bytes []byte
	var err error

	if path.Ext(c.Filename) == ".yaml" {
		bytes, err = yaml.Marshal(a)
	} else {
		bytes, err = json.Marshal(a)
	}

	if err != nil {
		return err
	}

	c.Lock()
	defer c.Unlock()

	c.AppConfig = a
	return ioutil.WriteFile(c.Filename, bytes, 0644)
}

func (c *ConfigFile) LoadConfig(ac interface{}, overwrite bool, initCB func() (interface{}, error)) (*ConfigFile, error) {
	c.Lock()

	bytes, err := ioutil.ReadFile(c.Filename)
	if overwrite || err != nil {
		c.Unlock()
		if err != nil {
			log.Error(err.Error())
		}

		p, err := initCB()
		if err != nil {
			return nil, err
		}
		err = c.SaveExplicit(p)
		if err != nil {
			return nil, err
		}

		return nil, err
	}
	c.Unlock()

	if path.Ext(c.Filename) == ".yaml" {
		err = yaml.Unmarshal(bytes, ac)
	} else {
		err = json.Unmarshal(bytes, ac)
	}
	if err != nil {
		return nil, err
	}

	c.AppConfig = ac

	return c, nil
}
