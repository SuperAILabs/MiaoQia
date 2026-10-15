"""M0真实组件测试启动器；只装配真实模块，业务处理不在此重写。"""
import importlib
import os
import signal
from miaoqia.kernel import db, tracing
from miaoqia.kernel.runtime import Env

name=os.environ['M0_WORKER']
tracing.setup(name, 'consumer')
d= db.Database(os.environ['M0_APP_DSN'])
module=importlib.import_module('miaoqia.'+name).module()
env=Env(role='consumer',brokers=os.environ['MIAOQIA_TEST_KAFKA'],db=d,subsystem=name)
c=module.consumers(env)[0]
c.name += '.'+os.environ['M0_RUN']
signal.signal(signal.SIGTERM,lambda *_:c.stop())
try: c.run()
finally:
    d.close()
    tracing.shutdown()
