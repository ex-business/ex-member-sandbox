"""从用户明确指定的本地佣金配置生成私有接入配置，不打印或覆盖凭据。"""
import argparse
import json
import os
from pathlib import Path
parser = argparse.ArgumentParser()
parser.add_argument('--commission-config', required=True, help='佣金沙箱的本地私有 runtime.json')
args = parser.parse_args()
root = Path(__file__).resolve().parents[1]
source = json.loads(Path(args.commission_config).expanduser().read_text())
if source.get('LOCAL_REGISTRATION_ENABLED') != 'true':
    raise SystemExit('佣金配置尚未启用注册接入，未生成配置')
config = {
    'Listen': '127.0.0.1:18280', 'PublicURL': 'http://127.0.0.1:18280/',
    'Database': str(root / '.local/members.db'),
    'CommissionURL': 'http://' + source['HTTP_ADDR'],
    'CommissionToken': source['LOCAL_OPERATOR_TOKEN'],
    'RegistrationKey': source['LOCAL_REGISTRATION_KEY'],
    'Tenant': source['LOCAL_TENANT'], 'App': source['LOCAL_APP'], 'Source': 'sandbox',
}
local = root / '.local'
local.mkdir(mode=0o700, exist_ok=True)
target = local / 'config.json'
with open(target, 'x', opener=lambda path, flags: os.open(path, flags, 0o600)) as handle:
    json.dump(config, handle, ensure_ascii=False, indent=2)
    handle.write('\n')
print('私有配置已生成到 .local/config.json，未输出凭据')
