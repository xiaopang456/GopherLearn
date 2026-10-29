# 隔离入口第一阶段

基于 303b5564d0d822bf7e7d21389e6f8c48ee670ebc。显式配置加载沿用原 defaultConfig 和校验，不读取环境覆盖或私有默认文件。数据库使用原 OpenConfigured，InitializeSchema 只转调原 migrate；只有显式 migrate=true 才接入初始化，当前施工没有运行它。生命周期按原 worker Start/Stop，并在退出新建 15 秒 context，部分启动失败按逆序清理。

第二阶段接通可执行入口与真实装配函数；本提交先固定配置、资源与生命周期基础。不会启用 v2_entry_reference 或改现有默认入口。
