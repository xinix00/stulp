#!/usr/bin/env python3
"""Full Stulp controller on a disposable HopOS volume, across SIGKILL.

Uses prebuilt kernel/Hop artifacts read-only; never builds or edits sibling repos.
Set STULP_QEMU_KERNEL and STULP_QEMU_HOP to select explicit artifacts.
"""
import functools
import base64
import hmac
import io
import zipfile
import http.cookiejar
import http.client
import hashlib
import http.server
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
TARGET = 'aarch64-unknown-none-softfloat'
BUNDLE = os.environ.get('STULP_QEMU_BUNDLE') == '1'
PLUGIN_BINARY = 'stulp-all-plugins-hopos' if BUNDLE else 'stulp-virtualdevices-hopos'
PLUGIN_IDS = ['com.stulp.' + name for name in ('virtualdevices', 'weather', 'somfy', 'nibe', 'spotify', 'notify', 'wiim', 'sigenergy', 'unifi', 'matter')] if BUNDLE else ['com.stulp.virtualdevices']
OUT = ROOT / ('target/qemu-stulp-bundle' if BUNDLE else 'target/qemu-stulp-controller')

class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *_):
        pass

def exercise(port,boot,start_plugin):
    cookies=http.cookiejar.CookieJar()
    browser=urllib.request.build_opener(urllib.request.HTTPCookieProcessor(cookies))
    base=f'http://127.0.0.1:{port}'
    def call(method,path,body=None,kind='application/json'):
        raw=body if isinstance(body,bytes) else None if body is None else json.dumps(body).encode()
        req=urllib.request.Request(base+path,data=raw,headers={'Content-Type':kind},method=method)
        with browser.open(req,timeout=30) as response:return response.read()
    page=call('GET','/qemu-key')
    assert b'<!DOCTYPE html>' in page or b'<!doctype html>' in page,page[:100]
    if boot==0:
        group=json.loads(call('POST','/api/stulp/device-groups',{'name':'QEMU persistent room'}))
        assert group['name']=='QEMU persistent room',group
        archived=call('GET','/api/stulp/backup')
        with zipfile.ZipFile(io.BytesIO(archived)) as z:
            assert json.loads(z.read('backup.json'))['format']==1
            assert b'QEMU persistent room' in z.read('stulp.json')
        # Seed only the disposable document, through the same restore API as the browser.
        with zipfile.ZipFile(io.BytesIO(archived)) as z:
            document=json.loads(z.read('stulp.json'));meta=json.loads(z.read('backup.json'))
        app='com.stulp.virtualdevices'
        document.setdefault('system',{})['fixturePadding']='x'*(1200*1024)
        document['apps']=[{'id':plugin_id,'enabled':True} for plugin_id in PLUGIN_IDS]
        document['devices']=[{'id':'switch','name':'QEMU lamp','appId':app,'driverId':'switch','data':{'id':'qemu'},'capabilities':['onoff'],'store':{'onoff':False}}]
        meta['apps']=[{'id':plugin_id} for plugin_id in PLUGIN_IDS]
        data=io.BytesIO()
        with zipfile.ZipFile(data,'w',compression=zipfile.ZIP_STORED) as z:
            z.writestr('backup.json',json.dumps(meta));z.writestr('stulp.json',json.dumps(document))
        result=json.loads(call('POST','/api/stulp/restore',data.getvalue(),'application/zip'))
        assert result['restored'],result
        connection=http.client.HTTPConnection('127.0.0.1',port,timeout=30)
        payload=data.getvalue()
        connection.request('POST','/api/stulp/restore',body=(payload[i:i+65536] for i in range(0,len(payload),65536)),headers={'Content-Type':'application/zip','Transfer-Encoding':'chunked','Cookie':'; '.join(c.name+'='+c.value for c in cookies)},encode_chunked=True)
        response=connection.getresponse();value=response.read();assert response.status==200,value
        assert json.loads(value)['restored'];connection.close()
        try:call('POST','/api/stulp/restore',b'broken ZIP','application/zip')
        except urllib.error.HTTPError as error:assert error.code==422
        else:raise AssertionError('corrupt backup was accepted')
    start_plugin()
    deadline=time.monotonic()+45
    while time.monotonic()<deadline:
        plugin=json.loads(call('GET','/api/manager/apps/app/com.stulp.virtualdevices'))
        if plugin['state']=='running':break
        time.sleep(.1)
    else:raise AssertionError('HopOS virtualdevices plugin did not initialize: '+str(plugin))
    if BUNDLE:
        deadline=time.monotonic()+60
        while time.monotonic()<deadline:
            states=json.loads(call('GET','/api/manager/apps/app'))
            if all(states.get(app_id,{}).get('state')=='running' for app_id in PLUGIN_IDS):break
            time.sleep(.2)
        else:raise AssertionError('Bundle apps not running: '+str({k:v.get('state') for k,v in states.items()}))
        print('PASS: all ten bundled plugins running',flush=True)
    device=json.loads(call('GET','/api/manager/devices/device/switch'))
    assert device['available'],device
    assert device['capabilitiesObj']['onoff']['value'] is (boot==1),device
    if boot==0:
        call('PUT','/api/manager/devices/device/switch/capability/onoff',{'value':True})
        device=json.loads(call('GET','/api/manager/devices/device/switch'))
        assert device['capabilitiesObj']['onoff']['value'] is True,device
        scene=json.loads(call('POST','/api/stulp/scenes',{'name':'QEMU scene','states':[{'deviceId':'switch','capabilityId':'onoff','value':False}]}))
        route='/api/manager/devices/device/scene:'+scene['id']+'/capability/onoff'
        call('PUT',route,{'value':True});call('PUT',route,{'value':False})
        device=json.loads(call('GET','/api/manager/devices/device/switch'))
        assert device['capabilitiesObj']['onoff']['value'] is True,device
        page=call('GET','/app-ui/com.stulp.virtualdevices/pair/switch/name.html')
        assert b'app-frame.css' in page and b'Stulp' in page,page[:100]
    groups=call('GET','/api/stulp/device-groups')
    assert b'QEMU persistent room' in groups,groups

def main():
    subprocess.run(['cargo','build','--locked','--release','--target',TARGET,'-p','stulp-hopos-app'],cwd=ROOT,check=True)
    subprocess.run(['cargo','build','--locked','--release','--target',TARGET,'-p','stulp-hopos-plugins','--bin',PLUGIN_BINARY],cwd=ROOT,check=True)
    meta=json.loads(subprocess.check_output(['cargo','metadata','--locked','--format-version','1'],cwd=ROOT))
    sdk=Path(next(p['manifest_path'] for p in meta['packages'] if p['name']=='applib')).parent.parent
    kernel=Path(os.environ.get('STULP_QEMU_KERNEL',str(sdk/'target'/TARGET/'release/hopos')))
    hop=Path(os.environ.get('STULP_QEMU_HOP',str(ROOT.parent/'hop/hop/target'/TARGET/'release/agentd-hopos')))
    binary=ROOT/'target'/TARGET/'release/stulp-hopos-app'
    plugin=ROOT/'target'/TARGET/'release'/PLUGIN_BINARY
    for path in [kernel,hop,binary,plugin]:
        if not path.is_file():
            raise SystemExit('Required prebuilt artifact missing: '+str(path))
    sysroot=Path(subprocess.check_output(['rustc','--print','sysroot'],cwd=ROOT,text=True).strip())
    objcopy=next(sysroot.glob('lib/rustlib/*/bin/rust-objcopy'),None)
    if objcopy is None:
        raise SystemExit('rust-objcopy missing')
    qemu=shutil.which('qemu-system-aarch64')
    if qemu is None:
        raise SystemExit('qemu-system-aarch64 missing')
    OUT.mkdir(parents=True,exist_ok=True)
    (OUT/'checks.json').unlink(missing_ok=True)
    evidence={'artifacts':{str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in [kernel,hop,binary,plugin]}}
    with tempfile.TemporaryDirectory(prefix='stulp-qemu-') as directory:
        temporary=Path(directory)
        staged=temporary/'hop.elf'
        app=temporary/'stulp.elf'
        for source,dest in [(hop,staged),(binary,app),(plugin,temporary/'plugin.elf')]:
            subprocess.run([str(objcopy),'--strip-debug',str(source),str(dest)],check=True)
        size=staged.stat().st_size
        if size>14680064:
            raise SystemExit('Hop exceeds staging size')
        disk=temporary/'disk.img'
        with disk.open('wb') as f: f.truncate(64<<20)
        server=http.server.ThreadingHTTPServer(('127.0.0.1',0),functools.partial(Quiet,directory=str(temporary)))
        server.daemon_threads=True
        thread=threading.Thread(target=server.serve_forever,daemon=True)
        thread.start()
        held=[]
        for _ in range(5):
            sock=socket.socket();sock.bind(('127.0.0.1',0));held.append(sock)
        ports=[sock.getsockname()[1] for sock in held]
        forward=','.join(f'hostfwd=tcp:127.0.0.1:{host}-:{guest}' for host,guest in zip(ports,[10100,8080,9080,8081,7000]))
        command=[qemu,'-M','virt,gic-version=3,highmem-ecam=off,virtualization=on','-cpu','cortex-a53','-smp','4','-m','3G',
            '-display','none','-monitor','none','-serial','stdio','-global','virtio-mmio.force-legacy=false',
            '-device','virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0','-netdev','user,id=n0,'+forward,
            '-drive',f'if=none,id=disk0,file={disk},format=raw', '-device','virtio-blk-device,drive=disk0,bus=virtio-mmio-bus.1',
            '-kernel',str(kernel),'-device',f'loader,file={staged},addr=0xb0200000,force-raw=on',
            '-device',f'loader,addr=0xb0100000,data={size},data-len=8','-device','loader,addr=0xb0100008,data=1,data-len=8']
        job={'name':'stulp-hopos-app','driver':'hop','artifacts':[{'url':f'http://10.0.2.2:{server.server_port}/stulp.elf'}],
            'memory_limit':268435456,'volumes':{'/volumes/stulp-hopos-app':'/data'},'ports':{'http':8081,'attach':7000},'env':{'STULP_TOKEN':'qemu-key','STULP_ATTACH_SECRET':'qemu-secret','STULP_ENTROPY_SEED':base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip('=')}}
        token=base64.urlsafe_b64encode(hmac.new(b'qemu-secret',b'token\0com.stulp.virtualdevices\0',hashlib.sha256).digest()).decode().rstrip('=')
        plugin_job={'name':'stulp-virtualdevices','driver':'hop','artifacts':[{'url':f'http://10.0.2.2:{server.server_port}/plugin.elf'}],
            'memory_limit':134217728,'env':{'STULP_ATTACH':'10.100.0.3:7000','STULP_ATTACH_TOKEN':token,'STULP_ENTROPY_SEED':base64.urlsafe_b64encode(os.urandom(32)).decode().rstrip('=')}}
        if BUNDLE:
            job.update(cores=1, tags={'sharegroup':'huis'}, memory_limit=48<<20)
            plugin_job.update(cores=1, tags={'sharegroup':'huis'}, memory_limit=64<<20)
            plugin_job['env'].pop('STULP_ATTACH_TOKEN')
            plugin_job['env']['STULP_ATTACH_SECRET']='qemu-secret'
        def start_plugin():
            req=urllib.request.Request(f'http://127.0.0.1:{ports[2]}/v1/jobs',data=json.dumps(plugin_job).encode(),headers={'Content-Type':'application/json'},method='POST')
            with urllib.request.urlopen(req,timeout=15) as response:assert response.status in [200,201,202]
        for sock in held:sock.close()
        try:
            for boot,marker in enumerate(['STULP_CONTROLLER_READY','STULP_CONTROLLER_READY']):
                log=OUT/f'boot-{boot}.log'
                with log.open('w') as output:
                    process=subprocess.Popen(command,stdin=subprocess.DEVNULL,stdout=output,stderr=subprocess.STDOUT,start_new_session=True)
                    try:
                        deadline=time.monotonic()+90
                        posted=(boot==1) # Hop restores the controller job from its own volume
                        while time.monotonic()<deadline:
                            text=log.read_text(errors='replace')
                            if any(m in text for m in ['STULP_CONTROLLER_FAIL','HOPOS_PANIC','HOPOS_EXCEPTION','HOPOS_APP_PANIC']):
                                raise RuntimeError('Failure marker: '+str(log))
                            if not posted and 'HOP_LEADER' in text and 'HOP_UP' in text:
                                request=urllib.request.Request(f'http://127.0.0.1:{ports[2]}/v1/jobs',data=json.dumps(job).encode(),headers={'Content-Type':'application/json'},method='POST')
                                with urllib.request.urlopen(request,timeout=15) as response:
                                    if response.status not in [200,201,202]:raise RuntimeError('Job refused')
                                posted=True
                            if marker in text:
                                exercise(ports[3],boot,start_plugin)
                                if boot==0:
                                    # This older prebuilt Hop races two saved jobs onto the
                                    # same cold slot. Exercise Stulp's hard restart with one
                                    # saved job, then attach a fresh plugin on the next boot.
                                    req=urllib.request.Request(f'http://127.0.0.1:{ports[2]}/v1/jobs/stulp-virtualdevices',method='DELETE')
                                    with urllib.request.urlopen(req,timeout=15) as response:
                                        assert response.status in [200,202,204]
                                    time.sleep(12) # persist the scheduler's removal as well
                                print(f'PASS boot {boot}: controller HTTP, plugin + backup/restore',flush=True)
                                break
                            if process.poll() is not None:raise RuntimeError('QEMU exited: '+str(log))
                            time.sleep(.1)
                        else:raise RuntimeError('Deadline: '+str(log))
                    finally:
                        if process.poll() is None:os.killpg(process.pid,signal.SIGKILL)
                        process.wait(timeout=5)
        finally:
            server.shutdown();server.server_close();thread.join(timeout=5)
    evidence.update(bundle=BUNDLE,plugin_count=len(PLUGIN_IDS),controller=True,http=True,backup_restore=True,cold_restore=True,rust_plugin=True,capabilities=True,scenes=True,plugin_ui=True)
    (OUT/'checks.json').write_text(json.dumps(evidence,indent=2)+'\n')
    print('PASS: HopOS controller served the original UI and persisted through hard QEMU stop; evidence in '+str(OUT))

if __name__=='__main__':main()
