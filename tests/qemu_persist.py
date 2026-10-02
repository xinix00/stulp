#!/usr/bin/env python3
"""Stulp's actual A/B storage on a disposable HopOS volume, across SIGKILL.

Uses prebuilt kernel/Hop artifacts read-only; never builds or edits sibling repos.
Set STULP_QEMU_KERNEL and STULP_QEMU_HOP to select explicit artifacts.
"""
import functools
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
OUT = ROOT / 'target/qemu-stulp-persist'

class Quiet(http.server.SimpleHTTPRequestHandler):
    def log_message(self, *_):
        pass

def main():
    subprocess.run(['cargo','build','--locked','--release','--target',TARGET,'-p','stulp-persist-check'],cwd=ROOT,check=True)
    meta=json.loads(subprocess.check_output(['cargo','metadata','--locked','--format-version','1'],cwd=ROOT))
    sdk=Path(next(p['manifest_path'] for p in meta['packages'] if p['name']=='applib')).parent.parent
    kernel=Path(os.environ.get('STULP_QEMU_KERNEL',str(sdk/'target'/TARGET/'release/hopos')))
    hop=Path(os.environ.get('STULP_QEMU_HOP',str(ROOT.parent/'hop/hop/target'/TARGET/'release/agentd-hopos')))
    binary=ROOT/'target'/TARGET/'release/stulp-persist-check'
    for path in [kernel,hop,binary]:
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
    evidence={'artifacts':{str(p):hashlib.sha256(p.read_bytes()).hexdigest() for p in [kernel,hop,binary]}}
    with tempfile.TemporaryDirectory(prefix='stulp-qemu-') as directory:
        temporary=Path(directory)
        staged=temporary/'hop.elf'
        app=temporary/'stulp.elf'
        for source,dest in [(hop,staged),(binary,app)]:
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
        for _ in range(3):
            sock=socket.socket();sock.bind(('127.0.0.1',0));held.append(sock)
        ports=[sock.getsockname()[1] for sock in held]
        forward=','.join(f'hostfwd=tcp:127.0.0.1:{host}-:{guest}' for host,guest in zip(ports,[10100,8080,9080]))
        command=[qemu,'-M','virt,gic-version=3,highmem-ecam=off,virtualization=on','-cpu','cortex-a53','-smp','4','-m','3G',
            '-display','none','-monitor','none','-serial','stdio','-global','virtio-mmio.force-legacy=false',
            '-device','virtio-net-device,netdev=n0,bus=virtio-mmio-bus.0','-netdev','user,id=n0,'+forward,
            '-drive',f'if=none,id=disk0,file={disk},format=raw', '-device','virtio-blk-device,drive=disk0,bus=virtio-mmio-bus.1',
            '-kernel',str(kernel),'-device',f'loader,file={staged},addr=0xb0200000,force-raw=on',
            '-device',f'loader,addr=0xb0100000,data={size},data-len=8','-device','loader,addr=0xb0100008,data=1,data-len=8']
        job={'name':'stulp-persist-check','driver':'hop','artifacts':[{'url':f'http://10.0.2.2:{server.server_port}/stulp.elf'}],
            'memory_limit':33554432,'volumes':{'/volumes/stulp-persist-check':'/data'}}
        for sock in held:sock.close()
        try:
            for boot,marker in enumerate(['STULP_PERSIST_WRITE','STULP_PERSIST_READ']):
                log=OUT/f'boot-{boot}.log'
                with log.open('w') as output:
                    process=subprocess.Popen(command,stdin=subprocess.DEVNULL,stdout=output,stderr=subprocess.STDOUT,start_new_session=True)
                    try:
                        deadline=time.monotonic()+90
                        posted=False
                        while time.monotonic()<deadline:
                            text=log.read_text(errors='replace')
                            if any(m in text for m in ['STULP_PERSIST_FAIL','HOPOS_PANIC','HOPOS_EXCEPTION','HOPOS_APP_PANIC']):
                                raise RuntimeError('Failure marker: '+str(log))
                            if boot==1 and 'STULP_PERSIST_WRITE' in text:
                                raise RuntimeError('Durable document missing after cold boot')
                            if not posted and 'HOP_LEADER' in text and 'HOP_UP' in text:
                                request=urllib.request.Request(f'http://127.0.0.1:{ports[2]}/v1/jobs',data=json.dumps(job).encode(),headers={'Content-Type':'application/json'},method='POST')
                                with urllib.request.urlopen(request,timeout=15) as response:
                                    if response.status not in [200,201,202]:raise RuntimeError('Job refused')
                                posted=True
                            if marker in text:
                                print(f'PASS boot {boot}: {marker}',flush=True)
                                break
                            if process.poll() is not None:raise RuntimeError('QEMU exited: '+str(log))
                            time.sleep(.1)
                        else:raise RuntimeError('Deadline: '+str(log))
                    finally:
                        if process.poll() is None:os.killpg(process.pid,signal.SIGKILL)
                        process.wait(timeout=5)
        finally:
            server.shutdown();server.server_close();thread.join(timeout=5)
    evidence.update(sync_rpc=True,cold_restore=True,payload_bytes=192*1024)
    (OUT/'checks.json').write_text(json.dumps(evidence,indent=2)+'\n')
    print('PASS: Stulp document survived hard QEMU stop; evidence in '+str(OUT))

if __name__=='__main__':main()
