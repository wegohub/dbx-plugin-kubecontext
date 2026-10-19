#!/usr/bin/env python3
"""Black-box JSON-RPC acceptance. Only temporary kubeconfigs are modified."""
import json
import os
from pathlib import Path
import queue
import socket
import subprocess
import tempfile
import threading
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
class RPC:
    def __init__(self, binary):
        self.process = subprocess.Popen([str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, encoding='utf-8')
        self.inbox = queue.Queue()
        self.events = []
        self.seq = 0
        def read():
            for line in self.process.stdout:
                self.inbox.put(json.loads(line))
        threading.Thread(target=read, daemon=True).start()
    def call(self, method, params, expected_error=None):
        self.seq += 1
        self.process.stdin.write(json.dumps(dict(jsonrpc='2.0', id=self.seq, method=method, params=params))+'\n')
        self.process.stdin.flush()
        while True:
            item = self.inbox.get(timeout=20)
            if 'id' not in item:
                self.events.append(item)
                continue
            assert item['id'] == self.seq, item
            if expected_error is not None:
                assert item.get('error',{}).get('code')==expected_error,item
                return item['error']
            assert 'error' not in item, item.get('error')
            return item.get('result')
    def event(self, method, stream=None, timeout=10):
        end = time.monotonic()+timeout
        while time.monotonic()<end:
            for index, item in enumerate(self.events):
                if item['method'] == method and (stream is None or item['params'].get('streamId') == stream):
                    return self.events.pop(index)['params']
            self.events.append(self.inbox.get(timeout=max(.01,end-time.monotonic())))
        raise AssertionError(f'missing event {method}')
    def output_until(self, stream, text):
        out=''
        end=time.monotonic()+10
        while text not in out:
            try: out+=self.event('terminal/output',stream,timeout=max(.01,end-time.monotonic()))['data']
            except queue.Empty: raise AssertionError(f'Expected {text!r}; terminal output: {out!r}')
        return out
    def close(self):
        self.process.terminate()
        self.process.wait(timeout=5)

def main():
    with tempfile.TemporaryDirectory(prefix='kc-acceptance-') as tmp:
        tmp=Path(tmp)
        binary=tmp/'backend'
        subprocess.run(['go','build','-o',str(binary),'.'],cwd=ROOT/'backend',check=True)
        with socket.socket() as sock:
            sock.bind(('127.0.0.1',0)); port=sock.getsockname()[1]
        mock=subprocess.Popen(['python3',str(ROOT/'examples/mock-cluster/mock_kube_api.py'),str(port)],stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,env={**os.environ,'KC_MOCK_ANONYMOUS':'1'})
        rpc=RPC(binary)
        try:
            for _ in range(100):
                try:
                    urllib.request.urlopen(f'http://127.0.0.1:{port}/version',timeout=1)
                    break
                except Exception: time.sleep(.02)
            path=tmp/'config'
            path.write_text((ROOT/'examples/mock-cluster/kubeconfig.yaml').read_text().replace(':18080',f':{port}'))
            connection=dict(id='acceptance',name='Acceptance',external_config=dict(kubeconfig_path=str(path),namespace='legacy-wrong'))
            rpc.call('plugin/initialize',dict(host=dict(protocolVersions=[1])))
            rpc.call('connection/connect',dict(connection=connection))
            params=dict(connectionId='acceptance',context='mock-prod')
            nodes=rpc.call('kube/contexts',params)['contexts']
            assert len(nodes)==2
            assert all('token' not in json.dumps(n) for n in nodes)
            print('PASS aggregate connection and context dropdown data')
            for method in ['kube/delete','kube/scale','kube/restart']:
                rpc.call(method,dict(**params,kind='pods',namespace='default',name='example',read_only=False),expected_error=-32013)
            print('PASS resource mutations are permanently rejected, including legacy read_only=false')
            for name,namespace in [('mock-prod','default'),('mock-staging','kube-system'),('mock-prod','default')]:
                scope=dict(connectionId='acceptance',context=name)
                overview=rpc.call('kube/overview',scope)
                assert overview['namespace']==namespace,overview
                listing=rpc.call('kube/list',dict(**scope,kind='pods'))
                assert listing['namespace']==namespace,listing
                permissions=rpc.call('kube/permissions',scope)
                assert permissions['namespace']==namespace,permissions
            print('PASS per-context namespaces override legacy connection namespace for overview, list and permissions')
            rpc.call('kube/use-context',params)
            rows=rpc.call('kube/list',dict(**params,kind='pods',namespace='default'))['rows']
            pod=rows[0]['name']
            resource=rpc.call('kube/get',dict(**params,kind='pods',namespace='default',name=pod))
            assert json.loads(resource['json'])['metadata']['name']==pod
            print('PASS resource details include kind and return full object')
            rpc.call('kube/logs/start',dict(**params,namespace='default',name=pod,streamId='log-test',tailLines=100))
            logs=[]
            while not any('实时中文日志' in line for line in logs):
                logs+=rpc.event('kube/logChunk','log-test')['lines']
            assert not any('\x1b' in line for line in logs)
            # A live chunk must arrive before the stream ends, not after collecting the response.
            assert not any(e['method']=='kube/logEnd' for e in rpc.events)
            rpc.call('kube/logs/stop',dict(**params,streamId='log-test'))
            rpc.event('kube/logEnd','log-test')
            print('PASS live UTF-8 log chunks, ANSI stripping, cancellation')
            for context,stream in [('mock-prod','term-a'),('mock-staging','term-b')]:
                rpc.call('terminal/start',dict(connectionId='acceptance',context=context,streamId=stream,cols=100,rows=25))
            rpc.call('kube/use-context',dict(connectionId='acceptance',context='mock-staging'))
            for stream,context in [('term-a','mock-prod'),('term-b','mock-staging')]:
                # Split a marker to distinguish shell echo from executed output.
                command="printf 'MARK%s' 'ER '; kubectl config current-context\r"
                rpc.call('terminal/input',dict(connectionId='acceptance',streamId=stream,data=command))
                output=rpc.output_until(stream,'MARKER '+context)
                assert 'MARKER '+context in output, output
                rpc.call('terminal/resize',dict(connectionId='acceptance',streamId=stream,cols=120,rows=30))
            rpc.call('terminal/input',dict(connectionId='acceptance',streamId='term-a',data="kubectl get pod\r"))
            rpc.output_until('term-a',pod)
            rpc.call('terminal/input',dict(connectionId='acceptance',streamId='term-a',data="sleep 30\r"))
            time.sleep(.15)
            rpc.call('terminal/input',dict(connectionId='acceptance',streamId='term-a',data='\x03'))
            rpc.call('terminal/input',dict(connectionId='acceptance',streamId='term-a',data="printf 'AFTER%s\\n' 'INTERRUPT'\r"))
            rpc.output_until('term-a','AFTERINTERRUPT')
            for stream in ['term-a','term-b']:
                rpc.call('terminal/stop',dict(connectionId='acceptance',streamId=stream))
                rpc.event('terminal/exit',stream)
            print('PASS two persistent PTYs, context isolation, kubectl without --context, resize and Ctrl-C')
            usage=rpc.call('kube/pod-usage',dict(**params,namespace='default'))
            assert usage['rows'] and usage['rows'][0]['cpuMillicores']==125,usage
            metrics=rpc.call('kube/pod-metrics',dict(**params,namespace='default',name=pod))
            assert metrics['cpuMillicores']==125 and metrics['memoryBytes']==134217728,metrics
            assert metrics['diskUsedBytes']==1048576 and metrics['network']['rxBytes']>0,metrics
            print('PASS Pod usage and monitor CPU/memory/disk/network through GET-only APIs')
            incoming=(ROOT/'examples/mock-cluster/kubeconfig.yaml').read_text().replace('mock-prod','new-prod').replace('mock-staging','new-staging')
            merged=rpc.call('kube/merge',dict(**params,kubeconfig=incoming))
            assert len(merged['contexts'])==4
            rpc.event('kube/contextsChanged')
            assert len(rpc.call('kube/contexts',params)['contexts'])==4
            assert list(tmp.glob('config.bak-*'))
            print('PASS merge preserves contexts, creates backup and emits context refresh')
            imported=tmp/'source.yaml'
            imported.write_text(incoming.replace('new-prod','imported-prod').replace('new-staging','imported-staging'))
            rpc.call('kube/contexts',params)
            rpc.events=[e for e in rpc.events if e.get('method')!='kube/contextsChanged']
            draft={**connection,'id':'new-draft','external_config':{**connection['external_config'],'import_path':str(imported)}}
            action=rpc.call('connection/action',dict(connection=draft,action=dict(id='import-kubeconfig')))
            assert action['success'] and action['fieldValues']['import_path']==''
            fields={field['key'] for field in json.loads((ROOT/'manifest.json').read_text())['contributions'][0]['fields']}
            assert set(action['fieldValues']) <= fields, 'import returned undeclared form fields'
            refreshed={rpc.event('kube/contextsChanged')['connectionId'],rpc.event('kube/contextsChanged')['connectionId']}
            assert refreshed=={'new-draft','acceptance'}, refreshed
            assert len(rpc.call('kube/contexts',params)['contexts'])==6
            print('PASS new-connection import action merges source and clears pending input')
            rpc.call('connection/disconnect',dict(connection=connection))
        finally:
            rpc.close(); mock.terminate(); mock.wait(timeout=5)

if __name__=='__main__': main()
