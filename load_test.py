import requests
import random
import string
import time
import threading
import statistics
from concurrent.futures import ThreadPoolExecutor
import json
from datetime import datetime

# Configuration
BASE_URL = "http://localhost:5000/api/v1/dht"
TEST_DURATION = 300  # 5 minutes in seconds
CONCURRENT_WORKERS = 10
REQUEST_DELAY = 0.1  # Delay between requests in seconds

# Global stats
stats = {
    "total_requests": 0,
    "successful_requests": 0,
    "failed_requests": 0,
    "response_times": [],
    "endpoint_stats": {},
    "errors": []
}

def generate_random_key(length=10):
    """Generate a random string for testing."""
    return ''.join(random.choices(string.ascii_letters + string.digits, k=length))

def make_request(method, endpoint, data=None, params=None):
    """Make an HTTP request and record stats."""
    url = f"{BASE_URL}{endpoint}"
    start_time = time.time()
    
    try:
        if method == "GET":
            response = requests.get(url, params=params)
        elif method == "POST":
            response = requests.post(url, json=data)
        else:
            raise ValueError(f"Unsupported method: {method}")
        
        duration = time.time() - start_time
        
        # Record stats
        with threading.Lock():
            stats["total_requests"] += 1
            stats["response_times"].append(duration)
            
            if endpoint not in stats["endpoint_stats"]:
                stats["endpoint_stats"][endpoint] = {
                    "count": 0,
                    "success": 0,
                    "fail": 0,
                    "times": []
                }
            
            stats["endpoint_stats"][endpoint]["count"] += 1
            stats["endpoint_stats"][endpoint]["times"].append(duration)
            
            if response.status_code < 400:
                stats["successful_requests"] += 1
                stats["endpoint_stats"][endpoint]["success"] += 1
            else:
                stats["failed_requests"] += 1
                stats["endpoint_stats"][endpoint]["fail"] += 1
                stats["errors"].append({
                    "endpoint": endpoint,
                    "status": response.status_code,
                    "error": response.text,
                    "timestamp": datetime.now().isoformat()
                })
                
        return response
    
    except Exception as e:
        duration = time.time() - start_time
        with threading.Lock():
            stats["total_requests"] += 1
            stats["failed_requests"] += 1
            stats["response_times"].append(duration)
            stats["errors"].append({
                "endpoint": endpoint,
                "status": 0,
                "error": str(e),
                "timestamp": datetime.now().isoformat()
            })
        return None

def test_list_peers():
    """Test listing DHT peers."""
    make_request("GET", "/peers")

def test_routing_table():
    """Test getting routing table info."""
    make_request("GET", "/routing")

def test_query_key():
    """Test querying for closest peers."""
    key = generate_random_key()
    make_request("GET", f"/query/{key}")

def test_provide_content():
    """Test providing content to the DHT."""
    # Using a random CID-like string for testing
    cid = f"Qm{generate_random_key(44)}"
    make_request("POST", f"/provide/{cid}")

def test_find_providers():
    """Test finding providers for content."""
    cid = f"Qm{generate_random_key(44)}"
    make_request("GET", f"/findprovs/{cid}")

def test_find_peer():
    """Test finding a peer by ID."""
    # First get a list of peers to try finding
    response = make_request("GET", "/peers")
    if response and response.status_code == 200:
        peers = response.json()
        if peers:
            peer_id = random.choice(peers)["ID"]
            make_request("GET", f"/findpeer/{peer_id}")

def test_get_value():
    """Test getting a value from DHT."""
    key = generate_random_key()
    make_request("GET", f"/get/{key}")

def test_put_value():
    """Test putting a value into DHT."""
    key = generate_random_key()
    value = {"value": generate_random_key(100)}
    make_request("POST", f"/put/{key}", data=value)

def test_bootstrap():
    """Test bootstrapping the DHT."""
    make_request("POST", "/bootstrap")

def test_metrics():
    """Test getting DHT metrics."""
    make_request("GET", "/metrics")

def test_closest_peers():
    """Test finding closest peers to a key."""
    key = generate_random_key()
    make_request("GET", f"/closest/{key}")

def worker():
    """Worker thread that continuously makes requests."""
    test_functions = [
        test_list_peers,
        test_routing_table,
        test_query_key,
        test_provide_content,
        test_find_providers,
        test_find_peer,
        test_get_value,
        test_put_value,
        test_bootstrap,
        test_metrics,
        test_closest_peers
    ]
    
    while time.time() - start_time < TEST_DURATION:
        # Randomly select a test function
        test_func = random.choice(test_functions)
        test_func()
        time.sleep(REQUEST_DELAY)

def print_stats():
    """Print statistics during the test."""
    while time.time() - start_time < TEST_DURATION:
        time.sleep(10)
        with threading.Lock():
            print("\n=== Current Stats ===")
            print(f"Total Requests: {stats['total_requests']}")
            print(f"Successful: {stats['successful_requests']}")
            print(f"Failed: {stats['failed_requests']}")
            
            if stats['response_times']:
                avg_time = sum(stats['response_times']) / len(stats['response_times'])
                print(f"Avg Response Time: {avg_time:.3f}s")
                
            print("\nEndpoint Breakdown:")
            for endpoint, data in stats['endpoint_stats'].items():
                success_rate = (data['success'] / data['count']) * 100 if data['count'] > 0 else 0
                avg_time = sum(data['times']) / len(data['times']) if data['times'] else 0
                print(f"{endpoint}: {data['count']} requests, {success_rate:.1f}% success, avg {avg_time:.3f}s")

def save_results():
    """Save test results to a file."""
    filename = f"load_test_results_{datetime.now().strftime('%Y%m%d_%H%M%S')}.json"
    
    # Calculate final stats
    final_stats = {
        "test_duration_seconds": TEST_DURATION,
        "concurrent_workers": CONCURRENT_WORKERS,
        "request_delay": REQUEST_DELAY,
        "total_requests": stats["total_requests"],
        "successful_requests": stats["successful_requests"],
        "failed_requests": stats["failed_requests"],
        "request_rate": stats["total_requests"] / TEST_DURATION,
        "response_times": {
            "average": statistics.mean(stats["response_times"]) if stats["response_times"] else 0,
            "median": statistics.median(stats["response_times"]) if stats["response_times"] else 0,
            "min": min(stats["response_times"]) if stats["response_times"] else 0,
            "max": max(stats["response_times"]) if stats["response_times"] else 0,
            "p95": statistics.quantiles(stats["response_times"], n=20)[-1] if stats["response_times"] else 0,
        },
        "endpoint_stats": stats["endpoint_stats"],
        "errors": stats["errors"]
    }
    
    with open(filename, "w") as f:
        json.dump(final_stats, f, indent=2)
    
    print(f"\nTest results saved to {filename}")

if __name__ == "__main__":
    print(f"Starting load test with {CONCURRENT_WORKERS} workers for {TEST_DURATION} seconds...")
    start_time = time.time()
    
    # Start stats printer
    stats_thread = threading.Thread(target=print_stats)
    stats_thread.daemon = True
    stats_thread.start()
    
    # Run workers
    with ThreadPoolExecutor(max_workers=CONCURRENT_WORKERS) as executor:
        futures = [executor.submit(worker) for _ in range(CONCURRENT_WORKERS)]
        
        # Wait for test duration
        time.sleep(TEST_DURATION)
        
        # Cancel any remaining tasks
        for future in futures:
            future.cancel()
    
    # Save results
    save_results()
    
    print("Load test completed!")