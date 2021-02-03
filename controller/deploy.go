package controller

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"gitlab.eoitek.net/EOI/ckman/config"
	"gitlab.eoitek.net/EOI/ckman/deploy"
	"gitlab.eoitek.net/EOI/ckman/log"
	"gitlab.eoitek.net/EOI/ckman/model"
	"gitlab.eoitek.net/EOI/ckman/service/clickhouse"
	"gitlab.eoitek.net/EOI/ckman/service/nacos"
)

type DeployController struct {
	config *config.CKManConfig
	nacosClient *nacos.NacosClient
}

func NewDeployController(config *config.CKManConfig, nacosClient *nacos.NacosClient) *DeployController {
	deploy := &DeployController{}
	deploy.config = config
	deploy.nacosClient = nacosClient
	return deploy
}

func DeployPackage(d deploy.Deploy, base *deploy.DeployBase, conf interface{}) (int, error) {
	log.Logger.Infof("start init deploy")
	if err := d.Init(base, conf); err != nil {
		return model.INIT_PACKAGE_FAIL, err
	}

	log.Logger.Infof("start copy packages")
	if err := d.Prepare(); err != nil {
		return model.PREPARE_PACKAGE_FAIL, err
	}

	log.Logger.Infof("start install packages")
	if err := d.Install(); err != nil {
		return model.INSTALL_PACKAGE_FAIL, err
	}

	log.Logger.Infof("start copy config files")
	if err := d.Config(); err != nil {
		return model.CONFIG_PACKAGE_FAIL, err
	}

	log.Logger.Infof("start service")
	if err := d.Start(); err != nil {
		return model.START_PACKAGE_FAIL, err
	}

	log.Logger.Infof("start check service")
	if err := d.Check(); err != nil {
		return model.CHECK_PACKAGE_FAIL, err
	}

	return model.SUCCESS, nil
}

// @Summary 部署clickhouse
// @Description 部署clickhouse
// @version 1.0
// @Security ApiKeyAuth
// @Param req body model.DeployCkReq true "request body"
// @Failure 200 {string} json "{"code":400,"msg":"请求参数错误","data":""}"
// @Failure 200 {string} json "{"code":5011,"msg":"初始化组件失败","data":""}"
// @Failure 200 {string} json "{"code":5012,"msg":"准备组件失败","data":""}"
// @Failure 200 {string} json "{"code":5013,"msg":"安装组件失败","data":""}"
// @Failure 200 {string} json "{"code":5014,"msg":"配置组件失败","data":""}"
// @Failure 200 {string} json "{"code":5015,"msg":"启动组件失败","data":""}"
// @Failure 200 {string} json "{"code":5016,"msg":"检查组件启动状态失败","data":""}"
// @Success 200 {string} json "{"code":200,"msg":"success","data":nil}"
// @Router /api/v1/deploy/ck [post]
func (d *DeployController) DeployCk(c *gin.Context) {
	var req model.DeployCkReq
	var factory deploy.DeployFactory
	packages := make([]string, 3)

	if err := model.DecodeRequestBody(c.Request, &req); err != nil {
		model.WrapMsg(c, model.INVALID_PARAMS, model.GetMsg(model.INVALID_PARAMS), err.Error())
		return
	}

	packages[0] = fmt.Sprintf("%s-%s-%s", model.CkCommonPackagePrefix, req.ClickHouse.PackageVersion, model.CkCommonPackageSuffix)
	packages[1] = fmt.Sprintf("%s-%s-%s", model.CkServerPackagePrefix, req.ClickHouse.PackageVersion, model.CkServerPackageSuffix)
	packages[2] = fmt.Sprintf("%s-%s-%s", model.CkClientPackagePrefix, req.ClickHouse.PackageVersion, model.CkClientPackageSuffix)
	factory = deploy.CKDeployFacotry{}
	ckDeploy := factory.Create()
	base := &deploy.DeployBase{
		Hosts:    req.Hosts,
		Packages: packages,
		User:     req.User,
		Password: req.Password,
	}

	code, err := DeployPackage(ckDeploy, base, &req.ClickHouse)
	if err != nil {
		model.WrapMsg(c, code, model.GetMsg(code), err.Error())
		return
	}

	conf := convertCkConfig(&req)
	conf.Mode = model.CkClusterDeploy
	data, err := d.nacosClient.GetConfig()
	if err != nil {
		model.WrapMsg(c, model.GET_NACOS_CONFIG_FAIL, model.GetMsg(model.GET_NACOS_CONFIG_FAIL), err.Error())
		return
	}
	if data != "" {
		clickhouse.UpdateLocalCkClusterConfig([]byte(data))
	}
	clickhouse.CkClusters.Store(req.ClickHouse.ClusterName, conf)
	clickhouse.AddCkClusterConfigVersion()
	buf, _ := clickhouse.MarshalClusters()
	clickhouse.WriteClusterConfigFile(buf)
	d.nacosClient.PublishConfig(string(buf))
	model.WrapMsg(c, model.SUCCESS, model.GetMsg(model.SUCCESS), nil)
}

func convertCkConfig(req *model.DeployCkReq) model.CKManClickHouseConfig {
	conf := model.CKManClickHouseConfig{
		Port:        req.ClickHouse.CkTcpPort,
		User:        req.ClickHouse.User,
		Password:    req.ClickHouse.Password,
		Cluster:     req.ClickHouse.ClusterName,
		ZkNodes:     req.ClickHouse.ZkNodes,
		ZkPort:      req.ClickHouse.ZkPort,
		Version:     req.ClickHouse.PackageVersion,
		SshUser:     req.User,
		SshPassword: req.Password,
		Shards:      req.ClickHouse.Shards,
		Path:        req.ClickHouse.Path,
	}

	hosts := make([]string, 0)
	hostnames := make([]string, 0)
	for _, shard := range req.ClickHouse.Shards {
		if len(shard.Replicas) > 1 {
			conf.IsReplica = true
		}
		for _, replica := range shard.Replicas {
			hosts = append(hosts, replica.Ip)
			hostnames = append(hostnames, replica.HostName)
		}
	}
	conf.Hosts = hosts
	conf.Names = hostnames

	clickhouse.CkConfigFillDefault(&conf)
	return conf
}
