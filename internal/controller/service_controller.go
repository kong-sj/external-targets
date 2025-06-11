package controller

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

/*
- //+kubebuilder:rbac 마커들은 make manifests명령어를 통해 controller-gen이 자동으로 인식하여 confg/rbac/role.yaml에 정의됨
- external-targets Operator의 Service Object 접근 권한 설정
- Service의 status 필드에 대한 읽기 권한과 Service 자체에 대한 생성, 업데이트, 수정 등에 대한 권한 설정
*/
//+kubebuilder:rbac:groups=core,resources=services/status,verbs=get
//+kubebuilder:rbac:groups=core,resources=services,verbs=create;update;patch;delete;get;list;watch

const (
	externalTargetsNamespace = "external-targets" // ExternalName 서비스를 생성/관리할 네임스페이스 (미리 생성 필요)
	lbLinkerFinalizer        = "lb-linker/finalizer"
)

// ServiceReconciler reconciles a Service object
type ServiceReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Log    logr.Logger
}

// sanitizeName은 쿠버네티스 오브젝트 이름 규칙 정제
func (r *ServiceReconciler) sanitizeName(name string) string {
	name = strings.ToLower(name)
	name = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(name, "-")
	if len(name) > 63 { // Kubernetes리소스 이른은 63까지만 허용되므로 최대 길이 63자로 제힌
		name = name[:63]
	}
	name = strings.Trim(name, "-") // 이름의 앞뒤에 붙은 하이픈 제거
	if name == "" {
		// 매우 예외적인 경우, 기본 이름 또는 UUID 사용 고려
		return "default-ext-name-" + time.Now().Format("20060102150405")
	}
	return name
}

func (r *ServiceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("service", req.NamespacedName)

	// 1. 원본 Service 객체 가져오기
	var originalService corev1.Service
	if err := r.Get(ctx, req.NamespacedName, &originalService); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Original Service not found. Ignoring since we are not handling deletions in this simplified version.")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get Original Service")
		return ctrl.Result{}, err
	}

	// 2. Service가 LoadBalancer 타입인지 확인
	if originalService.Spec.Type != corev1.ServiceTypeLoadBalancer {
		log.Info("Service is not LoadBalancer type, ignoring.", "type", originalService.Spec.Type)
		// 만약 이전에 이 서비스에 대해 ExternalName을 만들었다면, 여기서 정리 로직이 필요하지만 단순화 버전에서는 생략
		return ctrl.Result{}, nil
	}

	// 3. Service가 삭제중인지 확인 (DeletionTimestamp가 0이 아닌경우 삭제중인 것으로 판단)
	if !originalService.ObjectMeta.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&originalService, lbLinkerFinalizer) {
			log.Info("LoadBalancer Service is being deleted, starting cleanup logic.")
			// Target정리가 필요한 ExternalName은 LB의 이름규칙을 기반으로 확인하기 위해 변수 설정
			externalNameServiceName := fmt.Sprintf("%s-%s-ext", originalService.Name, originalService.Namespace)
			externalNameServiceName = r.sanitizeName(externalNameServiceName)

			// Target을 정리할 ExternalName을 가져온다.
			var externalNameSvcToClear corev1.Service
			err := r.Get(ctx, types.NamespacedName{Name: externalNameServiceName, Namespace: externalTargetsNamespace}, &externalNameSvcToClear)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// 정리할 ExternalName이 없다면 곧바로 Finalizer를 제거해도 되니 Log출력
					log.Info("Associated ExternalName SErvice Not found, nothing to clenup. delete finalizer")
				} else {
					// 기타오류의 경우 Reconcile 재시도
					log.Error(err, "Failed to get ExternalName Service for cleanup")
					return ctrl.Result{}, err
				}
			} else {
				// Externalname이 있는 경우, spec.externName 필드를 공백으로 변경
				log.Info("Found associated ExternalName Service, clearint its target.", "target", externalNameSvcToClear.Name)
				beforePatch := externalNameSvcToClear.DeepCopy()
				externalNameSvcToClear.Spec.ExternalName = "" // Target 공백 설정

				// Patch를 사용하여 변경내용 적용
				if err := r.Patch(ctx, &externalNameSvcToClear, client.MergeFrom(beforePatch)); err != nil {
					log.Error(err, "Failed to patch ExternalName Service to clear target")
					return ctrl.Result{}, err
				}
				log.Info("Successfully cleared ExternalName target.")
			}
		}
	}

	// 4. LoadBalancer의 외부 IP 또는 Hostname 정보 확인
	if len(originalService.Status.LoadBalancer.Ingress) == 0 {
		log.Info("LoadBalancer Ingress not yet available for Original Service.")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil // 잠시 후 다시 시도
	}

	var externalTarget string
	ingress := originalService.Status.LoadBalancer.Ingress[0]
	if ingress.Hostname != "" {
		externalTarget = ingress.Hostname
	} else if ingress.IP != "" {
		externalTarget = ingress.IP
	} else {
		log.Info("LoadBalancer Ingress found, but no Hostname or IP available yet for Original Service.")
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil
	}
	log.Info("Found external target for Original Service", "target", externalTarget)

	// 5. ExternalName Service 이름 결정(LB이름 + Namespace + ext) 예) my-sample-lb-3-default-ext
	externalNameServiceName := fmt.Sprintf("%s-%s-ext", originalService.Name, originalService.Namespace)
	externalNameServiceName = r.sanitizeName(externalNameServiceName) // 이름 규칙에 맞게 정제

	// 6. 기존 ExternalName Service 가져오기 시도
	foundExternalNameSvc := &corev1.Service{}
	externalNameSvcKey := types.NamespacedName{Name: externalNameServiceName, Namespace: externalTargetsNamespace}

	err := r.Get(ctx, externalNameSvcKey, foundExternalNameSvc)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// 6a. ExternalName Service가 없음: 새로 생성
			log.Info("ExternalName Service not found. Creating a new one.", "targetNamespace", externalTargetsNamespace, "targetName", externalNameServiceName)
			newExternalNameSvc := &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Name:      externalNameServiceName,
					Namespace: externalTargetsNamespace,
					Labels: map[string]string{
						"app.kubernetes.io/managed-by": "lb-linker-operator",
						"original-service-name":        originalService.Name,
						"original-service-namespace":   originalService.Namespace,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:         corev1.ServiceTypeExternalName,
					ExternalName: externalTarget,
				},
			}

			if errCreate := r.Create(ctx, newExternalNameSvc); errCreate != nil {
				log.Error(errCreate, "Failed to create new ExternalName Service")
				return ctrl.Result{}, errCreate
			}
			log.Info("Successfully created new ExternalName Service", "externalName", externalTarget)
			return ctrl.Result{}, nil
		}
		// 기타 Get 오류
		log.Error(err, "Failed to get ExternalName Service")
		return ctrl.Result{}, err
	}

	// 6b. ExternalName Service가 이미 존재함: spec.externalName 필드 업데이트 확인
	if foundExternalNameSvc.Spec.ExternalName != externalTarget {
		log.Info("Existing ExternalName Service needs update.", "oldTarget", foundExternalNameSvc.Spec.ExternalName, "newTarget", externalTarget)

		// 변경 전 객체의 DeepCopy 생성
		beforeUpdateExternalNameSvc := foundExternalNameSvc.DeepCopy()

		// 실제 객체 필드 변경
		foundExternalNameSvc.Spec.ExternalName = externalTarget

		// r.Patch를 사용하여 변경된 부분만 업데이트 진행
		if errPatch := r.Patch(ctx, foundExternalNameSvc, client.MergeFrom(beforeUpdateExternalNameSvc)); errPatch != nil {
			log.Error(errPatch, "Failed to patch existing ExternalName Service")
			return ctrl.Result{}, errPatch
		}
		log.Info("Successfully patched ExternalName Service's externalName field.", "from", beforeUpdateExternalNameSvc.Spec.ExternalName, "to", foundExternalNameSvc.Spec.ExternalName)
	} else {
		log.Info("ExternalName Service is already up-to-date.")
	}

	return ctrl.Result{}, nil
}

func (r *ServiceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Service{}).
		Complete(r)
}
